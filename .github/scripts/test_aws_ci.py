#!/usr/bin/env python3
"""Contract tests for the AWS CI substrate (DES-HOR-591-01).

These tests are hermetic: they never call AWS. They lock the tag scheme, the
least-privilege policy boundary, the per-run host-trust rendering, the launch
surface, and the reaper decisions that the smoke and reaper workflows execute.
"""

from __future__ import annotations

import contextlib
import datetime as dt
import io
import json
from pathlib import Path
import re
import unittest

from aws_ci import (
    ALL_APPROVED_INSTANCE_TYPES,
    APPROVED_INSTANCE_TYPES,
    AUTHORIZED_KEY_DELIMITER,
    CI_REGION,
    CI_REGIONS,
    DATA_VOLUME_DEVICE,
    DATA_VOLUME_DEVICE_NAMES,
    DEADLINE_TAG,
    DESCRIBE_ACTIONS,
    HOST_KEY_DELIMITER,
    HOST_KEY_PUB_DELIMITER,
    GITHUB_OWNER_ID,
    GITHUB_REPOSITORY_ID,
    MARKER_TAG,
    MAX_POLICY_CHARACTERS,
    NAME_TAG,
    RUN_TAG,
    SCENARIO_TAG,
    SSHD_DELIMITER,
    SSH_USER,
    AwsCiError,
    access_denied_action,
    build_parser,
    by_id_glob,
    cpu_instance_type,
    data_volume_id_from_instance,
    denied_case_command,
    denied_launch_cases,
    device_probe_command,
    error_class,
    gpu_instance_types,
    host_trust_entry,
    identity_probe_command,
    instance_deadline,
    instance_marker,
    launch_command,
    openssh_sha256_fingerprint,
    parse_ami_ids,
    parse_timestamp,
    policy_document,
    reap_plan,
    render_role_trust_policy,
    render_user_data,
    region_order,
    required_tags,
    ssh_command,
    tag_specifications,
    verify_required_tags,
)

ROOT = Path(__file__).resolve().parents[2]
TEST_PUBLIC_KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN8Yq1rY7oP4bJ0Vq0QKkRzZCkCgCkYcCq2Qq7r5jL0K iterabase-ci-test"
TEST_FINGERPRINT = "SHA256:ONHR/9Dok5xRYKqBjS3ohr2IW0WLHz74dXyNyvgCALI"
PLACEHOLDER_KEY_BODY = "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW"
# Assembled from fragments so secret scanners do not read the fixture as an
# OpenSSH private key; it is a placeholder body, not key material.
PEM_BEGIN = "-----BEGIN " + "OPENSSH PRIVATE KEY-----"
PEM_END = "-----END " + "OPENSSH PRIVATE KEY-----"
TEST_PRIVATE_KEY = f"{PEM_BEGIN}\n{PLACEHOLDER_KEY_BODY}\n{PEM_END}"
ACCOUNT_ID = "123456789012"
NOW = dt.datetime(2026, 9, 27, 12, 0, tzinfo=dt.timezone.utc)


def ci_instance() -> dict:
    return {
        "InstanceId": "i-ci",
        "InstanceType": "m6i.xlarge",
        "LaunchTime": "2026-09-27T09:00:00+00:00",
        "State": {"Name": "running"},
        "Tags": [
            {"Key": MARKER_TAG, "Value": "true"},
            {"Key": RUN_TAG, "Value": "12345"},
            {"Key": NAME_TAG, "Value": "iterabase-ci-12345-smoke-cpu"},
        ],
    }


class TagContractTests(unittest.TestCase):
    def test_required_tags_carry_the_marker_run_and_scenario(self) -> None:
        tags = required_tags("12345", "smoke-cpu")
        self.assertEqual(
            tags,
            [
                {"Key": MARKER_TAG, "Value": "true"},
                {"Key": RUN_TAG, "Value": "12345"},
                {"Key": SCENARIO_TAG, "Value": "smoke-cpu"},
                {"Key": NAME_TAG, "Value": "iterabase-ci-12345-smoke-cpu"},
            ],
        )
        self.assertNotIn(DEADLINE_TAG, [tag["Key"] for tag in tags])

    def test_deadline_tag_uses_rfc3339_utc(self) -> None:
        tags = required_tags("12345", "reaper-fixture", deadline=NOW)
        self.assertEqual(tags[-1], {"Key": DEADLINE_TAG, "Value": "2026-09-27T12:00:00Z"})

    def test_required_tags_reject_malformed_identity(self) -> None:
        for run_id, scenario in (("", "smoke-cpu"), ("12 3", "smoke-cpu"), ("12345", "smoke cpu")):
            with self.subTest(run_id=run_id, scenario=scenario):
                with self.assertRaises(AwsCiError):
                    required_tags(run_id, scenario)

    def test_tag_specifications_render_create_tags_input(self) -> None:
        self.assertEqual(
            tag_specifications("instance", required_tags("12345", "smoke-cpu")),
            "ResourceType=instance,Tags=[{Key=iterabase-ci,Value=true},"
            "{Key=iterabase-ci-run,Value=12345},{Key=iterabase-ci-scenario,Value=smoke-cpu},"
            "{Key=Name,Value=iterabase-ci-12345-smoke-cpu}]",
        )

    def test_instance_marker_reads_the_marker_only(self) -> None:
        self.assertEqual(instance_marker(ci_instance()["Tags"]), "true")
        self.assertIsNone(instance_marker([{"Key": NAME_TAG, "Value": "founder"}]))
        self.assertIsNone(instance_marker(None))


class HostTrustTests(unittest.TestCase):
    def test_fingerprint_matches_openssh_output(self) -> None:
        self.assertEqual(openssh_sha256_fingerprint(TEST_PUBLIC_KEY), TEST_FINGERPRINT)

    def test_known_hosts_entry_binds_address_port_and_key(self) -> None:
        entry = host_trust_entry("203.0.113.7", TEST_PUBLIC_KEY)
        self.assertEqual(
            entry,
            "203.0.113.7 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN8Yq1rY7oP4bJ0Vq0QKkRzZCkCgCkYcCq2Qq7r5jL0K",
        )
        # The bare host form is required for the default port: a bracketed `[host]:22`
        # entry never matches and produced "No ED25519 host key is known" in a real
        # smoke dispatch (verified against github.com too).
        self.assertNotIn("[", entry)
        with self.assertRaises(AwsCiError):
            host_trust_entry("host name", TEST_PUBLIC_KEY)

    def test_user_data_installs_only_the_per_run_identity(self) -> None:
        rendered = render_user_data(
            host_private_key=TEST_PRIVATE_KEY,
            host_public_key=TEST_PUBLIC_KEY,
            authorized_key=TEST_PUBLIC_KEY,
        )
        self.assertTrue(rendered.startswith("#!/bin/bash\n"))
        self.assertIn("set -euo pipefail", rendered)
        for delimiter in (
            HOST_KEY_DELIMITER,
            HOST_KEY_PUB_DELIMITER,
            AUTHORIZED_KEY_DELIMITER,
            SSHD_DELIMITER,
        ):
            self.assertEqual(rendered.count(f"<<'{delimiter}'"), 1)
            self.assertEqual(rendered.count(f"\n{delimiter}\n"), 1)
        self.assertIn("chmod 0600 /etc/ssh/ssh_host_ed25519_key", rendered)
        self.assertIn("/home/ubuntu/.ssh/authorized_keys", rendered)
        for line in ("PasswordAuthentication no", "PermitRootLogin no", "KbdInteractiveAuthentication no"):
            self.assertIn(line, rendered)
        self.assertIn("systemctl restart ssh", rendered)
        self.assertIn(TEST_PRIVATE_KEY, rendered)
        self.assertNotIn("KbdInteractiveAuthentication yes", rendered)

    def test_user_data_rejects_delimiter_injection(self) -> None:
        with self.assertRaises(AwsCiError):
            render_user_data(
                host_private_key=f"{PEM_BEGIN}\n{HOST_KEY_DELIMITER}\n{PEM_END}",
                host_public_key=TEST_PUBLIC_KEY,
                authorized_key=TEST_PUBLIC_KEY,
            )
        with self.assertRaises(AwsCiError):
            render_user_data(
                host_private_key="not-a-key",
                host_public_key=TEST_PUBLIC_KEY,
                authorized_key=TEST_PUBLIC_KEY,
            )

    def test_by_id_glob_strips_the_volume_dash(self) -> None:
        self.assertEqual(
            by_id_glob("vol-0123456789abcdef0"),
            "/dev/disk/by-id/*-Amazon_Elastic_Block_Store_vol0123456789abcdef0",
        )
        for invalid in ("i-0123456789abcdef0", "vol-xyz", "../vol-0123456789abcdef0"):
            with self.subTest(invalid=invalid):
                with self.assertRaises(AwsCiError):
                    by_id_glob(invalid)

    def test_device_probe_requires_exactly_one_whole_device(self) -> None:
        probe = device_probe_command("vol-0123456789abcdef0")
        self.assertIn(by_id_glob("vol-0123456789abcdef0"), probe)
        self.assertIn("shopt -s nullglob", probe)
        self.assertIn('"${#matches[@]}" -ne 1', probe)
        self.assertIn('test -b "$device"', probe)
        self.assertIn("lsblk -b -d -n -o SIZE", probe)

    def test_data_volume_identity_accepts_the_api_device_name_forms(self) -> None:
        for name in DATA_VOLUME_DEVICE_NAMES:
            with self.subTest(name=name):
                self.assertEqual(
                    data_volume_id_from_instance(
                        {
                            "InstanceId": "i-1",
                            "BlockDeviceMappings": [
                                {"DeviceName": "/dev/sda1", "Ebs": {"VolumeId": "vol-root"}},
                                {"DeviceName": name, "Ebs": {"VolumeId": "vol-data"}},
                            ],
                        }
                    ),
                    "vol-data",
                )
        with self.assertRaises(AwsCiError):
            data_volume_id_from_instance(
                {
                    "InstanceId": "i-1",
                    "BlockDeviceMappings": [{"DeviceName": "/dev/sda1", "Ebs": {"VolumeId": "vol-root"}}],
                }
            )
        with self.assertRaises(AwsCiError):
            data_volume_id_from_instance(
                {
                    "InstanceId": "i-1",
                    "BlockDeviceMappings": [
                        {"DeviceName": "/dev/sdf", "Ebs": {"VolumeId": "vol-a"}},
                        {"DeviceName": "/dev/xvdf", "Ebs": {"VolumeId": "vol-b"}},
                    ],
                }
            )

    def test_identity_probe_proves_only_the_per_run_host_key_is_live(self) -> None:
        probe = identity_probe_command()
        self.assertIn("ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub", probe)
        self.assertIn("test ! -e /etc/ssh/ssh_host_rsa_key", probe)
        self.assertIn("test ! -e /etc/ssh/ssh_host_ecdsa_key", probe)
        self.assertIn("per-run-identity", probe)

    def test_required_tags_helper_rejects_a_launch_without_them(self) -> None:
        tags = required_tags("12345", "smoke-cpu")
        self.assertEqual(
            verify_required_tags(tags, run_id="12345", scenario="smoke-cpu"),
            "iterabase-ci=true,iterabase-ci-run=12345,iterabase-ci-scenario=smoke-cpu,"
            "Name=iterabase-ci-12345-smoke-cpu",
        )
        for broken in (
            None,
            [],
            [tag for tag in tags if tag["Key"] != MARKER_TAG],
            [tag for tag in tags if tag["Key"] != RUN_TAG],
            tags + [{"Key": MARKER_TAG, "Value": "false"}],
        ):
            with self.subTest(broken=broken):
                with self.assertRaises(AwsCiError):
                    verify_required_tags(broken, run_id="12345", scenario="smoke-cpu")  # type: ignore[arg-type]

    def test_ssh_command_pins_the_host_key_and_forbids_interaction(self) -> None:
        command = ssh_command(
            key_path="/tmp/key",
            known_hosts_path="/tmp/known_hosts",
            address="203.0.113.7",
            remote_command="true",
        )
        for expected in ("StrictHostKeyChecking=yes", "UserKnownHostsFile=/tmp/known_hosts", "BatchMode=yes", "IdentitiesOnly=yes"):
            self.assertIn(expected, command)
        self.assertNotIn("StrictHostKeyChecking=no", command)
        self.assertNotIn("UserKnownHostsFile=/dev/null", command)
        # `--` only terminates options before the destination; after it the argument
        # would join the remote command on a non-permuting getopt (BSD or musl).
        self.assertNotIn("--", command)
        self.assertEqual(command[-2], f"{SSH_USER}@203.0.113.7")
        self.assertEqual(command[-1], "true")


class PolicyContractTests(unittest.TestCase):
    def setUp(self) -> None:
        self.document = policy_document(ACCOUNT_ID)
        self.statements = {statement["Sid"]: statement for statement in self.document["Statement"]}

    def test_rendered_policy_uses_only_the_fixed_region_and_account(self) -> None:
        with self.assertRaises(AwsCiError):
            policy_document("123", CI_REGION)
        with self.assertRaises(AwsCiError):
            policy_document(ACCOUNT_ID, "us-east-1")

    def test_policy_is_within_the_managed_policy_length_limit(self) -> None:
        self.assertLessEqual(len(json.dumps(self.document, separators=(",", ":"))), MAX_POLICY_CHARACTERS)

    def test_every_statement_is_rendered_and_well_formed(self) -> None:
        self.assertEqual(len(self.document["Statement"]), 22)
        for statement in self.document["Statement"]:
            self.assertIn(statement["Effect"], {"Allow", "Deny"})
            self.assertTrue(statement["Action"])
            self.assertTrue(statement["Resource"])

    def test_launch_allow_requires_ci_owned_ami_and_no_instance_profile(self) -> None:
        self.assertEqual(
            self.statements["LaunchFromCiOwnedAmi"]["Condition"],
            {"StringEquals": {"ec2:Owner": ACCOUNT_ID}},
        )
        self.assertEqual(
            self.statements["LaunchFromCiOwnedAmi"]["Resource"],
            [
                "arn:aws:ec2:*::image/*",
                f"arn:aws:ec2:*:{ACCOUNT_ID}:image/*",
                "arn:aws:ec2:*::snapshot/*",  # the GPU model cache, same owner condition (DES-HOR-590-04)
            ],
        )
        condition = self.statements["RunApprovedInstances"]["Condition"]
        self.assertEqual(condition["StringEquals"]["ec2:InstanceType"], list(ALL_APPROVED_INSTANCE_TYPES))
        self.assertEqual(condition["Null"]["ec2:InstanceProfile"], "true")

    def test_launch_requires_tags_on_the_instance_and_volume_resources(self) -> None:
        # AWS's documented require-tags-at-launch shape: the mandate sits on the
        # instance and volume resources (whose contexts carry aws:RequestTag), while
        # the plumbing resources are condition-free because conditions are evaluated
        # per resource and ec2:InstanceType exists only on the instance context.
        instance = self.statements["RunApprovedInstances"]
        self.assertEqual(instance["Resource"], f"arn:aws:ec2:*:{ACCOUNT_ID}:instance/*")
        condition = instance["Condition"]
        self.assertEqual(condition["StringEquals"]["ec2:InstanceType"], list(ALL_APPROVED_INSTANCE_TYPES))
        self.assertEqual(condition["StringEquals"][f"aws:RequestTag/{MARKER_TAG}"], "true")
        self.assertEqual(condition["Null"]["ec2:InstanceProfile"], "true")
        self.assertEqual(condition["Null"][f"aws:RequestTag/{RUN_TAG}"], "false")
        self.assertEqual(condition["ForAllValues:StringLike"]["aws:TagKeys"], [f"{MARKER_TAG}*", NAME_TAG])

        volumes = self.statements["RunApprovedVolumes"]
        self.assertEqual(volumes["Resource"], f"arn:aws:ec2:*:{ACCOUNT_ID}:volume/*")
        self.assertEqual(volumes["Condition"]["StringEquals"][f"aws:RequestTag/{MARKER_TAG}"], "true")
        self.assertEqual(volumes["Condition"]["Null"][f"aws:RequestTag/{RUN_TAG}"], "false")

        dependencies = self.statements["RunInstanceDependencies"]
        self.assertEqual(
            dependencies["Resource"],
            [
                f"arn:aws:ec2:*:{ACCOUNT_ID}:network-interface/*",
                f"arn:aws:ec2:*:{ACCOUNT_ID}:subnet/*",
                f"arn:aws:ec2:*:{ACCOUNT_ID}:key-pair/*",
                f"arn:aws:ec2:*:{ACCOUNT_ID}:spot-instances-request/*",
            ],
        )
        self.assertNotIn("Condition", dependencies)

    def test_tag_conditions_live_only_on_the_tagging_action(self) -> None:
        # aws:RequestTag is present for ec2:CreateTags (proven: the bootstrap copy's
        # tagging succeeded through these statements) and for the instance/volume
        # launch resources; the plumbing resources never carry it.
        for sid, statement in self.statements.items():
            if "ec2:RunInstances" not in json.dumps(statement["Action"]):
                continue
            if sid in {"RunApprovedInstances", "RunApprovedVolumes"}:
                continue
            with self.subTest(sid=sid):
                self.assertNotIn("aws:RequestTag", json.dumps(statement))
        for sid in ("TagCiResourcesOnCreate", "TagCopiedImagesOnCreate"):
            with self.subTest(sid=sid):
                condition = self.statements[sid]["Condition"]
                self.assertEqual(condition["StringEquals"][f"aws:RequestTag/{MARKER_TAG}"], "true")
                self.assertEqual(condition["Null"][f"aws:RequestTag/{RUN_TAG}"], "false")
                self.assertEqual(
                    condition["ForAllValues:StringLike"]["aws:TagKeys"],
                    [f"{MARKER_TAG}*", NAME_TAG],
                )

    def test_launch_allow_uses_the_tagged_ci_security_group_only(self) -> None:
        self.assertEqual(
            self.statements["UseOnlyCiSecurityGroup"]["Condition"],
            {"StringEquals": {f"ec2:ResourceTag/{MARKER_TAG}": "true"}},
        )

    def test_destructive_actions_are_tag_scoped(self) -> None:
        for sid in (
            "TerminateCiInstances",
            "ManageCiVolumes",
            "AttachVolumesToCiInstances",
            "RemoveCiImagesAndSnapshots",
        ):
            with self.subTest(sid=sid):
                self.assertEqual(
                    self.statements[sid]["Condition"],
                    {"StringEquals": {f"ec2:ResourceTag/{MARKER_TAG}": "true"}},
                )

    def test_describe_actions_are_read_only_and_account_wide(self) -> None:
        # DES-HOR-590-04: one read-only wildcard keeps the single policy in its size limit.
        self.assertEqual(self.statements["DescribeCiState"]["Action"], "ec2:Describe*")
        self.assertEqual(self.statements["DescribeCiState"]["Resource"], "*")

    def test_des_hor_590_04_amendments_stay_tag_scoped(self) -> None:
        # Bake: CreateImage only on a CI-marked instance.
        terminate = self.statements["TerminateCiInstances"]
        self.assertEqual(terminate["Action"], ["ec2:TerminateInstances", "ec2:CreateImage"])
        self.assertEqual(terminate["Condition"], {"StringEquals": {f"ec2:ResourceTag/{MARKER_TAG}": "true"}})
        # Model cache: copy sources must be CI-marked, copies carry the mandatory tags.
        self.assertIn("ec2:CopySnapshot", self.statements["RemoveCiImagesAndSnapshots"]["Action"])
        self.assertIn("ec2:CopySnapshot", self.statements["CreateCiSnapshots"]["Action"])
        self.assertEqual(
            self.statements["CreateCiSnapshots"]["Condition"]["StringEquals"][f"aws:RequestTag/{MARKER_TAG}"], "true"
        )
        self.assertIn("CopySnapshot", self.statements["TagCiResourcesOnCreate"]["Condition"]["StringEquals"]["ec2:CreateAction"])
        # Previews: only the deadline key may change on an existing CI instance.
        renew = self.statements["RenewCiDeadline"]
        self.assertEqual(renew["Action"], "ec2:CreateTags")
        self.assertEqual(renew["Resource"], f"arn:aws:ec2:*:{ACCOUNT_ID}:instance/*")
        self.assertEqual(renew["Condition"]["ForAllValues:StringEquals"], {"aws:TagKeys": [DEADLINE_TAG]})
        self.assertEqual(renew["Condition"]["StringEquals"], {f"ec2:ResourceTag/{MARKER_TAG}": "true"})

    def test_tag_on_create_is_bound_to_the_create_action(self) -> None:
        condition = self.statements["TagCiResourcesOnCreate"]["Condition"]
        self.assertIn("RunInstances", condition["StringEquals"]["ec2:CreateAction"])
        self.assertEqual(condition["StringEquals"][f"aws:RequestTag/{MARKER_TAG}"], "true")

    def test_no_iam_passrole_or_broad_write_surface_is_allowed(self) -> None:
        allowed_actions: list[str] = []
        for statement in self.document["Statement"]:
            if statement["Effect"] == "Allow":
                allowed_actions.extend(
                    [statement["Action"]] if isinstance(statement["Action"], str) else statement["Action"]
                )
        joined = " ".join(allowed_actions)
        self.assertNotIn("iam:", joined)
        self.assertNotIn("organizations:", joined)
        self.assertNotIn("s3:", joined)
        self.assertNotIn("ssm:", joined)
        self.assertNotIn("ec2:ModifyInstanceAttribute", joined)
        self.assertNotIn("ec2:StopInstances", joined)

    def test_explicit_denies_hold_the_boundary_independently_of_allows(self) -> None:
        denied = {
            sid: statement
            for sid, statement in self.statements.items()
            if statement["Effect"] == "Deny"
        }
        self.assertEqual(
            set(denied),
            {
                "DenyInstanceProfile",
                "DenyUnapprovedInstanceType",
                "DenyNonCiOwnedAmi",
                "DenyOutsideCiRegions",
                "DenyPrivilegeAndDataSurface",
            },
        )
        self.assertEqual(
            denied["DenyOutsideCiRegions"]["Condition"],
            {"StringNotEquals": {"aws:RequestedRegion": list(CI_REGIONS)}},
        )
        self.assertEqual(denied["DenyOutsideCiRegions"]["Action"], "ec2:*")
        self.assertEqual(
            denied["DenyUnapprovedInstanceType"]["Resource"],
            f"arn:aws:ec2:*:{ACCOUNT_ID}:instance/*",
        )
        self.assertEqual(
            denied["DenyNonCiOwnedAmi"]["Resource"],
            [
                "arn:aws:ec2:*::image/*",
                f"arn:aws:ec2:*:{ACCOUNT_ID}:image/*",
            ],
        )
        self.assertEqual(denied["DenyInstanceProfile"]["Condition"], {"Null": {"ec2:InstanceProfile": "false"}})
        self.assertEqual(
            denied["DenyUnapprovedInstanceType"]["Condition"]["StringNotEquals"]["ec2:InstanceType"],
            list(ALL_APPROVED_INSTANCE_TYPES),
        )
        self.assertEqual(
            denied["DenyNonCiOwnedAmi"]["Condition"]["StringNotEquals"]["ec2:Owner"],
            ACCOUNT_ID,
        )
        self.assertEqual(
            set(denied["DenyPrivilegeAndDataSurface"]["Action"]),
            {"iam:*", "organizations:*", "s3:*", "ssm:*", "sts:AssumeRole"},
        )

    def test_copied_images_are_tagged_through_an_empty_account_statement(self) -> None:
        # EC2 evaluates the copy's tag-on-create against wildcard image/snapshot ARNs
        # with an empty account (decoded from a real denial), so the copied AMI's
        # tags are enforced here rather than in the account-scoped statement.
        statement = self.statements["TagCopiedImagesOnCreate"]
        self.assertEqual(statement["Action"], "ec2:CreateTags")
        self.assertEqual(
            statement["Resource"],
            ["arn:aws:ec2:*::image/*", "arn:aws:ec2:*::snapshot/*"],
        )
        self.assertEqual(statement["Condition"]["StringEquals"]["ec2:CreateAction"], ["CopyImage"])
        self.assertEqual(
            statement["Condition"]["StringEquals"][f"aws:RequestTag/{MARKER_TAG}"],
            "true",
        )
        self.assertEqual(
            statement["Condition"]["Null"][f"aws:RequestTag/{RUN_TAG}"],
            "false",
        )

    # EC2 authorizes a copy against the source image and its source snapshot (for a
    # public AMI both are empty-account ARNs), and against destination wildcard ARNs
    # whose context carries no request tags; a tag condition here would deny every
    # copy, so the tags are enforced by TagCopiedImagesOnCreate instead.
    def test_copy_image_is_allowed_against_a_public_source_arn(self) -> None:
        # The smoke workflow's bootstrap copies a public Canonical image, and EC2
        # authorizes the copy against the source image ARN (empty account), so the
        # account-scoped pattern alone produced UnauthorizedOperation on CopyImage.
        # The decoded failures showed that neither evaluation carries
        # aws:RequestTag keys and the destination lacks the image attribute keys, so
        # the owner condition is the discriminator that pins the source while still
        # allowing the copy; the copied AMI's tags are enforced at ec2:CreateTags.
        statement = self.statements["CopyImagesIntoTheCiAccount"]
        self.assertEqual(statement["Action"], "ec2:CopyImage")
        self.assertEqual(
            statement["Resource"],
            [
                "arn:aws:ec2:*::image/*",
                "arn:aws:ec2:*::snapshot/*",
                f"arn:aws:ec2:*:{ACCOUNT_ID}:image/*",
                f"arn:aws:ec2:*:{ACCOUNT_ID}:snapshot/*",
            ],
        )
        # The source is pinned by owner: `ec2:Owner` is present in both copy
        # evaluations (amazon for the public Canonical source, this account for the
        # destination wildcard), so third-party images cannot be pulled in.
        self.assertEqual(
            statement["Condition"],
            {"StringEquals": {"ec2:Owner": ["amazon", ACCOUNT_ID]}},
        )
        self.assertNotIn("ec2:CopyImage", self.statements["BuildCiImagesAndSnapshots"]["Action"])
        tag_on_create = self.statements["TagCiResourcesOnCreate"]["Condition"]
        self.assertIn("CopyImage", tag_on_create["StringEquals"]["ec2:CreateAction"])
        self.assertEqual(
            tag_on_create["StringEquals"][f"aws:RequestTag/{MARKER_TAG}"],
            "true",
        )

    def test_region_boundary_is_enforced_by_the_policy_not_only_the_driver(self) -> None:
        # The ARNs carry a wildcard region (listing three regions blows the 6,144
        # character limit), so the allowed set is enforced by an explicit deny on the
        # global aws:RequestedRegion key and the driver's CI_REGIONS stays the search
        # order rather than the boundary.
        deny = self.statements["DenyOutsideCiRegions"]
        self.assertEqual(deny["Effect"], "Deny")
        self.assertEqual(deny["Action"], "ec2:*")
        self.assertEqual(deny["Resource"], "*")
        self.assertEqual(
            deny["Condition"]["StringNotEquals"]["aws:RequestedRegion"],
            list(CI_REGIONS),
        )

    def test_only_documented_statements_leave_the_ci_account(self) -> None:
        # Every allow is scoped to this account and region except the describe list
        # and the copy-source statement, whose public-image ARN is necessarily
        # empty-account; the deny statements are scoped separately by design.
        exempt = {
            "DescribeCiState",
            "LaunchFromCiOwnedAmi",
            "CopyImagesIntoTheCiAccount",
            "TagCopiedImagesOnCreate",
            "RemoveCiImagesAndSnapshots",
            # A cross-region model-cache copy lands on an empty-account snapshot ARN;
            # it still requires the mandatory request tags (DES-HOR-590-04).
            "CreateCiSnapshots",
        }
        for sid, statement in self.statements.items():
            if sid in exempt or statement["Effect"] == "Deny":
                continue
            resources = statement["Resource"]
            resources = [resources] if isinstance(resources, str) else resources
            for resource in resources:
                with self.subTest(sid=sid, resource=resource):
                    self.assertTrue(resource.startswith(f"arn:aws:ec2:*:{ACCOUNT_ID}:"))
                    self.assertNotEqual(resource, "*")

    def test_trust_policy_is_oidc_only_and_environment_free(self) -> None:
        document = render_role_trust_policy(ACCOUNT_ID)
        statement = document["Statement"][0]
        self.assertEqual(statement["Action"], "sts:AssumeRoleWithWebIdentity")
        self.assertEqual(
            statement["Principal"]["Federated"],
            f"arn:aws:iam::{ACCOUNT_ID}:oidc-provider/token.actions.githubusercontent.com",
        )
        self.assertEqual(statement["Condition"]["StringEquals"]["token.actions.githubusercontent.com:aud"], "sts.amazonaws.com")
        self.assertEqual(
            statement["Condition"]["StringLike"]["token.actions.githubusercontent.com:sub"],
            "repo:iterabase@338844113/iterabase-mono@1330311216:*",
        )
        self.assertNotIn(
            "repo:iterabase/iterabase-mono:*",
            json.dumps(document),
            "the classic subject form never matches an immutable-claim repository",
        )
        self.assertNotIn("environment", json.dumps(document))

    def test_trust_policy_pins_the_repository_and_owner_ids(self) -> None:
        statement = render_role_trust_policy(ACCOUNT_ID)["Statement"][0]
        subject = statement["Condition"]["StringLike"]["token.actions.githubusercontent.com:sub"]
        # GitHub's immutable subject claims carry the owner and repository IDs; the
        # rendered condition must pin them rather than trust a re-creatable name.
        self.assertIn(f"iterabase@{GITHUB_OWNER_ID}", subject)
        self.assertIn(f"iterabase-mono@{GITHUB_REPOSITORY_ID}", subject)
        self.assertTrue(subject.endswith(":*"))
        for overrides in (
            {"owner_id": "nuno"},
            {"repository_id": "main"},
        ):
            with self.subTest(overrides=overrides):
                with self.assertRaises(AwsCiError):
                    render_role_trust_policy(ACCOUNT_ID, **overrides)
        rendered = render_role_trust_policy(ACCOUNT_ID, owner_id="1", repository_id="2")
        self.assertIn(
            "repo:iterabase@1/iterabase-mono@2:*",
            json.dumps(rendered),
        )


class LaunchSurfaceTests(unittest.TestCase):
    def command(self) -> list[str]:
        return launch_command(
            region=CI_REGION,
            image_id="ami-0123456789abcdef0",
            instance_type=APPROVED_INSTANCE_TYPES["cpu"][0],
            subnet_id="subnet-0123456789abcdef0",
            security_group_id="sg-0123456789abcdef0",
            tags=required_tags("12345", "smoke-cpu"),
            user_data_path="/tmp/user-data.sh",
        )

    def test_launch_command_carries_the_approved_surface(self) -> None:
        command = self.command()
        joined = " ".join(command)
        self.assertIn("--instance-type m6i.xlarge", joined)
        self.assertIn("--instance-initiated-shutdown-behavior terminate", joined)
        self.assertIn("--associate-public-ip-address", joined)
        self.assertIn("--metadata-options HttpTokens=required,HttpEndpoint=enabled", joined)
        self.assertIn(f"DeviceName={DATA_VOLUME_DEVICE}", joined)
        self.assertIn("iterabase-ci", joined)
        self.assertIn("file:///tmp/user-data.sh", joined)

    def test_launch_command_never_attaches_an_instance_profile(self) -> None:
        joined = " ".join(self.command())
        self.assertNotIn("--iam-instance-profile", joined)

    def test_denied_cases_cover_every_required_denial(self) -> None:
        cases = denied_launch_cases(
            image_id="ami-0123456789abcdef0",
            public_ami_id="ami-0abcdef1234567890",
            instance_profile="iterabase-ci-denied-profile",
        )
        self.assertEqual(
            [case["name"] for case in cases],
            [
                "non-approved-instance-type",
                "missing-all-mandatory-tags",
                "missing-run-tag",
                "non-ci-account-ami",
                "instance-profile-attached",
            ],
        )
        by_name = {case["name"]: case for case in cases}
        self.assertEqual(by_name["non-approved-instance-type"]["instance_type"], "m6i.2xlarge")
        self.assertEqual(by_name["non-ci-account-ami"]["image_id"], "ami-0abcdef1234567890")
        self.assertEqual(by_name["instance-profile-attached"]["instance_profile"], "iterabase-ci-denied-profile")

    def test_denied_case_commands_isolate_one_violation_each(self) -> None:
        cases = {
            case["name"]: case
            for case in denied_launch_cases(
                image_id="ami-0123456789abcdef0",
                public_ami_id="ami-0abcdef1234567890",
                instance_profile="iterabase-ci-denied-profile",
            )
        }
        arguments = {
            name: denied_case_command(
                case,
                region=CI_REGION,
                run_id="12345",
                subnet_id="subnet-0123456789abcdef0",
                security_group_id="sg-0123456789abcdef0",
            )
            for name, case in cases.items()
        }
        for name, command in arguments.items():
            with self.subTest(case=name):
                joined = " ".join(command)
                self.assertIn("--subnet-id subnet-0123456789abcdef0", joined)
                self.assertIn("--security-group-ids sg-0123456789abcdef0", joined)
        self.assertNotIn("--tag-specifications", " ".join(arguments["missing-all-mandatory-tags"]))
        marker_only = " ".join(arguments["missing-run-tag"])
        self.assertIn("Key=iterabase-ci,Value=true", marker_only)
        self.assertNotIn(RUN_TAG, marker_only)
        profile_case = " ".join(arguments["instance-profile-attached"])
        self.assertIn("--iam-instance-profile Name=iterabase-ci-denied-profile", profile_case)
        foreign_ami = " ".join(arguments["non-ci-account-ami"])
        self.assertIn("--image-id ami-0abcdef1234567890", foreign_ami)
        self.assertIn("Key=iterabase-ci,Value=true", foreign_ami)

    def test_access_denied_action_is_extracted_from_the_aws_error(self) -> None:
        self.assertEqual(
            access_denied_action(
                "An error occurred (UnauthorizedOperation) when calling the RunInstances operation: "
                "You are not authorized to perform: ec2:RunInstances with an explicit deny in an identity-based policy"
            ),
            "ec2:RunInstances",
        )
        self.assertEqual(
            access_denied_action(
                "You are not authorized to perform: iam:PassRole on resource: "
                "arn:aws:iam::123456789012:role/example because no identity-based policy allows the iam:PassRole action"
            ),
            "iam:PassRole",
        )
        self.assertIsNone(access_denied_action("An error occurred (InvalidParameterValue)"))

    def test_error_class_is_recorded_for_denial_evidence(self) -> None:
        self.assertEqual(
            error_class(
                "\nAn error occurred (UnauthorizedOperation) when calling the RunInstances operation: "
                "You are not authorized to perform: ec2:RunInstances"
            ),
            "UnauthorizedOperation",
        )
        self.assertEqual(error_class("An error occurred (InvalidParameterValue) when calling the RunInstances operation"), "InvalidParameterValue")
        self.assertEqual(error_class("\naws: [ERROR]: Could not connect to the endpoint URL"), "unknown")


class ReaperTests(unittest.TestCase):
    def test_deadline_tag_takes_precedence_over_launch_age(self) -> None:
        instance = ci_instance()
        instance["Tags"] = instance["Tags"] + [{"Key": DEADLINE_TAG, "Value": "2026-09-27T13:00:00Z"}]
        self.assertEqual(
            instance_deadline(instance, 180),
            dt.datetime(2026, 9, 27, 13, 0, tzinfo=dt.timezone.utc),
        )

    def test_max_age_applies_without_a_deadline_tag(self) -> None:
        self.assertEqual(
            instance_deadline(ci_instance(), 180),
            dt.datetime(2026, 9, 27, 12, 0, tzinfo=dt.timezone.utc),
        )
        self.assertEqual(parse_timestamp("2026-09-27T09:00:00+00:00"), dt.datetime(2026, 9, 27, 9, 0, tzinfo=dt.timezone.utc))

    def test_plan_terminates_due_marked_instances_only(self) -> None:
        aged = ci_instance()
        aged["Tags"] = aged["Tags"] + [{"Key": DEADLINE_TAG, "Value": "2020-01-01T00:00:00Z"}]
        fresh = ci_instance()
        fresh["InstanceId"] = "i-fresh"
        fresh["LaunchTime"] = "2026-09-27T11:30:00+00:00"
        foreign = {
            "InstanceId": "i-foreign",
            "LaunchTime": "2026-01-01T00:00:00+00:00",
            "State": {"Name": "running"},
            "Tags": [{"Key": NAME_TAG, "Value": "founder-host"}],
        }
        plan = reap_plan([aged, fresh, foreign], now=NOW, max_age_minutes=180)
        self.assertEqual([item["instance_id"] for item in plan["terminate"]], [aged["InstanceId"]])
        self.assertEqual(plan["terminate"][0]["reason"], f"{DEADLINE_TAG} passed")
        self.assertEqual([item["instance_id"] for item in plan["pending"]], ["i-fresh"])
        self.assertEqual([item["instance_id"] for item in plan["foreign"]], ["i-foreign"])
        self.assertEqual(plan["errors"], [])

    def test_plan_terminates_a_marked_instance_that_exceeds_max_age(self) -> None:
        instance = ci_instance()
        instance["LaunchTime"] = "2026-09-27T08:59:00+00:00"
        plan = reap_plan([instance], now=NOW, max_age_minutes=180)
        self.assertEqual([item["instance_id"] for item in plan["terminate"]], ["i-ci"])
        self.assertEqual(plan["terminate"][0]["reason"], "max age reached")

    def test_plan_reports_instead_of_guessing_on_invalid_input(self) -> None:
        invalid = ci_instance()
        invalid["Tags"] = invalid["Tags"] + [{"Key": DEADLINE_TAG, "Value": "not-a-timestamp"}]
        missing_launch = ci_instance()
        missing_launch["InstanceId"] = "i-missing"
        del missing_launch["LaunchTime"]
        plan = reap_plan([invalid, missing_launch], now=NOW, max_age_minutes=180)
        self.assertEqual(plan["terminate"], [])
        self.assertEqual(plan["foreign"], [])
        self.assertEqual(len(plan["errors"]), 2)


class RepositoryContractTests(unittest.TestCase):
    def test_cli_exposes_only_used_subcommands(self) -> None:
        with contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(SystemExit):
                build_parser().parse_args(["resolve-ami"])
            for command in (
                "render-policy",
                "render-trust-policy",
                "verify-identity",
                "bootstrap-ami",
                "run-host",
                "denied-cases",
                "cleanup-run",
                "reap",
            ):
                with self.subTest(command=command):
                    with self.assertRaises(SystemExit) as raised:
                        build_parser().parse_args([command, "--help"])
                    self.assertEqual(raised.exception.code, 0)

    def test_every_ami_map_parse_threads_the_resolved_primary(self) -> None:
        # The bare-AMI form must never fall back to the module constant: cleanup-run
        # shipped that bug once, so every call site in the module is pinned here.
        source = (ROOT / ".github/scripts/aws_ci.py").read_text(encoding="utf-8")
        calls = re.findall(r"parse_ami_ids\((.*?)\)", source)
        self.assertGreaterEqual(len(calls), 3, "the parser and its call sites should be present")
        for call in calls:
            with self.subTest(call=call):
                if call.startswith("value: str"):
                    continue  # the definition itself
                self.assertIn(",", call, "parse_ami_ids must be called with an explicit primary region")

    def test_capacity_helpers_return_single_types_not_tuples(self) -> None:
        # A tuple passed where one instance type is expected produced a live
        # ParamValidation failure ("Values=('m6i.xlarge',)") in the denied-cases job.
        self.assertEqual(cpu_instance_type(), "m6i.xlarge")
        self.assertIsInstance(cpu_instance_type(), str)
        self.assertEqual(gpu_instance_types()[0], "g6.xlarge")
        for instance_type in gpu_instance_types():
            with self.subTest(instance_type=instance_type):
                self.assertIsInstance(instance_type, str)

    def test_region_order_puts_the_primary_first_and_covers_every_allowed_region(self) -> None:
        self.assertEqual(CI_REGIONS, ("eu-west-1", "eu-central-1", "eu-north-1"))
        self.assertEqual(region_order("eu-west-1"), CI_REGIONS)
        self.assertEqual(region_order("eu-central-1"), ("eu-central-1", "eu-west-1", "eu-north-1"))
        for region in CI_REGIONS:
            with self.subTest(region=region):
                self.assertEqual(region_order(region)[0], region)
                self.assertEqual(set(region_order(region)), set(CI_REGIONS))
        for invalid in ("us-east-1", "eu-west-2", "", "EU-WEST-1"):
            with self.subTest(invalid=invalid):
                with self.assertRaises(AwsCiError):
                    region_order(invalid)

    def test_ami_map_parses_and_rejects_unknown_regions(self) -> None:
        self.assertEqual(parse_ami_ids("ami-1"), {CI_REGION: "ami-1"})
        self.assertEqual(parse_ami_ids("ami-1", "eu-central-1"), {"eu-central-1": "ami-1"})
        self.assertEqual(parse_ami_ids("ami-1", "eu-north-1"), {"eu-north-1": "ami-1"})
        with self.assertRaises(AwsCiError):
            parse_ami_ids("ami-1", "us-east-1")
        self.assertEqual(
            parse_ami_ids("eu-north-1=ami-3, eu-west-1=ami-1,eu-central-1=ami-2"),
            {"eu-north-1": "ami-3", "eu-west-1": "ami-1", "eu-central-1": "ami-2"},
        )
        with self.assertRaises(AwsCiError):
            parse_ami_ids(" ")
        with self.assertRaises(AwsCiError):
            parse_ami_ids("us-east-1=ami-1")
        with self.assertRaises(AwsCiError):
            parse_ami_ids("")

    def test_gpu_allowlist_is_cheapest_first_and_all_sm86_class(self) -> None:
        # Every GPU type must carry a 24 GiB >= sm_86 accelerator: g6 = L4/sm_89,
        # g5 = A10G/sm_86. Order is the documented price order.
        self.assertEqual(
            APPROVED_INSTANCE_TYPES["gpu"],
            ("g6.xlarge", "g5.xlarge", "g6.2xlarge", "g5.2xlarge", "g5.4xlarge"),
        )
        self.assertEqual(APPROVED_INSTANCE_TYPES["cpu"], ("m6i.xlarge",))
        self.assertIn("g6.xlarge", ALL_APPROVED_INSTANCE_TYPES)
        self.assertNotIn("g4dn.xlarge", ALL_APPROVED_INSTANCE_TYPES, "T4 is sm_75 and cannot run the validated stack")

    def test_workflows_are_dispatch_only_and_use_the_repository_variables(self) -> None:
        smoke = (ROOT / ".github/workflows/aws-ci-smoke.yml").read_text(encoding="utf-8")
        reaper = (ROOT / ".github/workflows/reaper.yml").read_text(encoding="utf-8")
        self.assertIn("on:\n  workflow_dispatch:", smoke)
        self.assertNotIn("pull_request:", smoke)
        self.assertNotIn("\n  push:", smoke)
        self.assertIn("on:\n  schedule:\n    - cron:", reaper)
        self.assertIn("  workflow_dispatch:", reaper)
        for workflow in (smoke, reaper):
            with self.subTest(workflow=workflow.splitlines()[0]):
                self.assertIn("id-token: write", workflow)
                self.assertIn("vars.AWS_CI_ROLE_ARN", workflow)
                self.assertIn("vars.AWS_CI_REGION", workflow)
                self.assertIn(".github/scripts/aws_ci.py", workflow)
                self.assertIn("uses: ./.github/actions/setup-aws-ci", workflow)
                self.assertNotIn("AWS_SECRET_ACCESS_KEY", workflow)
                self.assertNotIn("aws-access-key-id", workflow)

    def test_actions_are_pinned_to_full_shas(self) -> None:
        targets = [
            *sorted((ROOT / ".github/workflows").glob("aws-ci-*.yml")),
            ROOT / ".github/actions/setup-aws-ci/action.yml",
        ]
        for path in targets:
            workflow = path.read_text(encoding="utf-8")
            for match in re.finditer(r"uses:\s+([^\s#]+)", workflow):
                target = match.group(1)
                if target.startswith("./"):
                    continue
                with self.subTest(path=path.name, target=target):
                    self.assertRegex(target, r"@[0-9a-f]{40}$")

    def test_oidc_setup_action_uses_the_committed_script_and_no_static_keys(self) -> None:
        action = (ROOT / ".github/actions/setup-aws-ci/action.yml").read_text(encoding="utf-8")
        self.assertIn("aws-actions/configure-aws-credentials@", action)
        self.assertIn("role-to-assume: ${{ inputs.role-arn }}", action)
        self.assertIn("audience: sts.amazonaws.com", action)
        self.assertIn("role-skip-session-tagging: \"true\"", action)
        self.assertIn("python3 .github/scripts/aws_ci.py verify-identity", action)
        self.assertIn("AWS_CI_ROLE_ARN", action)
        self.assertIn("AWS_CI_REGION", action)
        self.assertNotIn("actions/cache@", action)
        self.assertNotIn("aws-access-key-id", action)
        self.assertNotIn("aws-secret-access-key", action)

    def test_runbook_records_the_contract_and_validation_procedure(self) -> None:
        runbook = (ROOT / "docs/runbooks/aws-ci.md").read_text(encoding="utf-8")
        for expected in (
            "DES-HOR-591-01",
            "iterabase-ci-ssh",
            "AWS_CI_ROLE_ARN",
            "AWS_CI_REGION",
            "L-DB2E81BA",
            "L-1216C47A",
            "describe-instance-type-offerings",
            "aws-ci-smoke.yml",
            "reaper.yml",
        ):
            with self.subTest(expected=expected):
                self.assertIn(expected, runbook)


class FixtureLaunchTests(unittest.TestCase):
    """C1 fixtures, C6 previews and the DES-HOR-590-03 bake surface."""

    def test_fixture_launch_adds_root_model_cache_and_keeps_the_default_surface(self) -> None:
        import aws_ci

        base = dict(region=CI_REGION, image_id="ami-0123456789abcdef0", instance_type="g6.xlarge",
                    subnet_id="subnet-0123456789abcdef0", security_group_id="sg-0123456789abcdef0",
                    tags=required_tags("1", "gpu"), user_data_path="/tmp/u.sh")
        command = aws_ci.launch_command(**base, data_gib=30, root_gib=80,
                                        snapshot_volumes=(("/dev/sdg", "snap-0123456789abcdef0"),))
        mappings = command[command.index("--block-device-mappings") + 1:command.index("--tag-specifications")]
        self.assertEqual(mappings, [
            "DeviceName=/dev/sda1,Ebs={VolumeSize=80,VolumeType=gp3,DeleteOnTermination=true}",
            "DeviceName=/dev/sdf,Ebs={VolumeSize=30,VolumeType=gp3,DeleteOnTermination=true}",
            "DeviceName=/dev/sdg,Ebs={SnapshotId=snap-0123456789abcdef0,VolumeType=gp3,DeleteOnTermination=true}",
        ])
        self.assertNotIn("--instance-market-options", command)
        spot = aws_ci.launch_command(**base, spot=True)
        self.assertEqual(spot[spot.index("--instance-market-options") + 1], aws_ci.SPOT_MARKET_OPTIONS)
        self.assertIn("InstanceInterruptionBehavior=terminate", aws_ci.SPOT_MARKET_OPTIONS)

    def test_fixture_environment_is_exactly_what_the_forge_scenarios_read(self) -> None:
        import aws_ci

        forge_env = set(re.findall(
            r'"(FORGE_E2E_[A-Z_]+)"',
            (Path(__file__).resolve().parents[2] / "forge" / "test" / "e2e" / "host_fixture_test.go").read_text(encoding="utf-8"),
        ))
        host = aws_ci.PinnedHost(
            capacity="gpu", region=CI_REGION, instance_type="g6.xlarge", availability_zone="eu-west-1a",
            instance_id="i-0123456789abcdef0", ami_id="ami-0123456789abcdef0", public_ip="192.0.2.10",
            workdir=Path("/tmp/w"), ssh_key=Path("/tmp/w/id_ed25519"), known_hosts=Path("/tmp/w/known_hosts"),
            host_public="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDummyHostKeyMaterial iterabase-ci-gpu",
            data_volume_id="vol-0123456789abcdef0", capacity_failures=[],
        )
        environment = aws_ci.fixture_environment(
            host, data_device="/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0123456789abcdef0",
            manifest={"cache_root": "/var/lib/iterabase-e2e/image-cache", "generation": "abc"},
            model_device="/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0fedcba9876543210",
            model_uuid="2eb63d10-3d60-418e-bced-cae2f3a26f08",
        )
        self.assertTrue(forge_env <= set(environment), forge_env - set(environment))
        self.assertEqual(environment["FORGE_E2E_FIXTURE_SSH_USER"], "ubuntu")
        # The pinned host key is exported without its comment: exactly one OpenSSH key.
        self.assertEqual(len(environment["FORGE_E2E_FIXTURE_SSH_HOST_KEY"].split()), 2)
        with self.assertRaises(AwsCiError):
            aws_ci.fixture_environment(host, data_device="", manifest={"cache_root": "/c", "generation": "g"})

    def test_model_cache_mount_rejects_malformed_uuids(self) -> None:
        import aws_ci

        script = aws_ci.model_cache_mount_script("2eb63d10-3d60-418e-bced-cae2f3a26f08")
        self.assertIn("mount -o ro UUID=2eb63d10-3d60-418e-bced-cae2f3a26f08 /data/hf-cache", script)
        for bad in ("", "x; rm -rf /", "2eb63d10"):
            with self.subTest(uuid=bad), self.assertRaises(AwsCiError):
                aws_ci.model_cache_mount_script(bad)

    def test_model_cache_bake_proves_the_pinned_weight_hash(self) -> None:
        import aws_ci

        generation, authority = aws_ci.model_cache_generation()
        self.assertRegex(generation, r"^[0-9a-f]{16}$")
        script = aws_ci.model_cache_bake_script(authority, "/dev/disk/by-id/nvme-x")
        self.assertIn(authority["sha256"], script)
        self.assertIn(authority["revision"], script)
        self.assertIn(f"huggingface_hub=={aws_ci.HUGGINGFACE_HUB_VERSION}", script)

    def test_prune_keeps_the_newest_two_generations(self) -> None:
        import aws_ci

        def image(image_id: str, generation: str, created: str) -> dict:
            return {"ImageId": image_id, "CreationDate": created, "Tags": [{"Key": aws_ci.IMAGE_CACHE_TAG, "Value": generation}]}

        images = [image("ami-a", "g1", "2026-10-01"), image("ami-b", "g2", "2026-10-03"),
                  image("ami-c", "g3", "2026-10-05"), image("ami-d", "g3", "2026-10-04")]
        self.assertEqual([item["ImageId"] for item in aws_ci.prune_plan(images, aws_ci.IMAGE_CACHE_TAG)], ["ami-a"])
        self.assertEqual(aws_ci.prune_plan(images[:2], aws_ci.IMAGE_CACHE_TAG), [])

    def test_preview_inputs_fail_closed(self) -> None:
        import aws_ci

        for name in ("pr-1", "pr-133", "staging"):
            self.assertTrue(aws_ci.PREVIEW_NAME.match(name), name)
        for name in ("pr-0", "pr-", "prod", "pr-1;rm"):
            self.assertFalse(aws_ci.PREVIEW_NAME.match(name), name)
        self.assertIn("--hostname=iterabase-pr-7", aws_ci.tailscale_join_script("tskey-auth-kAbC123-XyZ", "pr-7"))
        with self.assertRaises(AwsCiError):
            aws_ci.tailscale_join_script("tskey-auth-x; curl evil", "pr-7")
        self.assertEqual(aws_ci.by_id_path("vol-0abc123"), "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0abc123")
        with self.assertRaises(AwsCiError):
            aws_ci.by_id_path("/dev/sdf")
        # Staging never expires on the preview TTL; PR previews do after 72 h.
        self.assertGreater(aws_ci.preview_deadline("staging") - aws_ci.preview_deadline("pr-1"), dt.timedelta(days=365))

    def test_bake_seal_removes_builder_identity(self) -> None:
        import aws_ci

        self.assertIn("rm -f /etc/ssh/ssh_host_* /home/ubuntu/.ssh/authorized_keys", aws_ci.BAKE_SEAL_SCRIPT)
        self.assertIn("cloud-init clean", aws_ci.BAKE_SEAL_SCRIPT)


if __name__ == "__main__":
    unittest.main()
