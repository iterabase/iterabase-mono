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
    APPROVED_INSTANCE_TYPES,
    AUTHORIZED_KEY_DELIMITER,
    CI_REGION,
    DATA_VOLUME_DEVICE,
    DATA_VOLUME_DEVICE_NAMES,
    DEADLINE_TAG,
    DESCRIBE_ACTIONS,
    HOST_KEY_DELIMITER,
    HOST_KEY_PUB_DELIMITER,
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
    data_volume_id_from_instance,
    denied_case_command,
    denied_launch_cases,
    device_probe_command,
    error_class,
    host_trust_entry,
    identity_probe_command,
    instance_deadline,
    instance_marker,
    launch_command,
    openssh_sha256_fingerprint,
    parse_timestamp,
    policy_document,
    reap_plan,
    render_role_trust_policy,
    render_user_data,
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
        self.assertEqual(
            host_trust_entry("203.0.113.7", TEST_PUBLIC_KEY),
            "[203.0.113.7]:22 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIN8Yq1rY7oP4bJ0Vq0QKkRzZCkCgCkYcCq2Qq7r5jL0K",
        )
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
        self.assertEqual(len(self.document["Statement"]), 18)
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
            f"arn:aws:ec2:{CI_REGION}:{ACCOUNT_ID}:image/*",
        )
        condition = self.statements["RunApprovedInstances"]["Condition"]
        self.assertEqual(condition["StringEquals"]["ec2:InstanceType"], sorted(APPROVED_INSTANCE_TYPES.values()))
        self.assertEqual(condition["Null"]["ec2:InstanceProfile"], "true")

    def test_launch_allow_requires_the_mandatory_marker_and_run_tags(self) -> None:
        condition = self.statements["RunApprovedInstances"]["Condition"]
        self.assertEqual(condition["StringEquals"][f"aws:RequestTag/{MARKER_TAG}"], "true")
        self.assertEqual(condition["Null"][f"aws:RequestTag/{RUN_TAG}"], "false")
        self.assertEqual(condition["ForAllValues:StringLike"]["aws:TagKeys"], [f"{MARKER_TAG}*", NAME_TAG])

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
        self.assertEqual(self.statements["DescribeCiState"]["Action"], DESCRIBE_ACTIONS)
        self.assertEqual(self.statements["DescribeCiState"]["Resource"], "*")
        for action in DESCRIBE_ACTIONS:
            self.assertTrue(action.startswith("ec2:Describe"), action)

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
                "DenyMissingMarkerTag",
                "DenyMissingRunTag",
                "DenyPrivilegeAndDataSurface",
            },
        )
        self.assertEqual(
            denied["DenyMissingMarkerTag"]["Condition"],
            {"StringNotEquals": {f"aws:RequestTag/{MARKER_TAG}": "true"}},
        )
        self.assertEqual(
            denied["DenyMissingRunTag"]["Condition"],
            {"Null": {f"aws:RequestTag/{RUN_TAG}": "true"}},
        )
        self.assertEqual(denied["DenyInstanceProfile"]["Condition"], {"Null": {"ec2:InstanceProfile": "false"}})
        self.assertEqual(
            denied["DenyUnapprovedInstanceType"]["Condition"]["StringNotEquals"]["ec2:InstanceType"],
            sorted(APPROVED_INSTANCE_TYPES.values()),
        )
        self.assertEqual(
            denied["DenyNonCiOwnedAmi"]["Condition"]["StringNotEquals"]["ec2:Owner"],
            ACCOUNT_ID,
        )
        self.assertEqual(
            set(denied["DenyPrivilegeAndDataSurface"]["Action"]),
            {"iam:*", "organizations:*", "s3:*", "ssm:*", "sts:AssumeRole"},
        )

    def test_each_mandatory_tag_has_its_own_single_key_deny(self) -> None:
        # IAM ANDs every key inside one condition block, so two mandatory tags in a
        # single Deny would only fire when both were missing. Each tag needs its own
        # statement, and this test keeps the conjunction from coming back.
        denies = [
            statement
            for statement in self.document["Statement"]
            if statement["Effect"] == "Deny"
            and statement["Action"] == "ec2:RunInstances"
            and "aws:RequestTag" in json.dumps(statement.get("Condition", {}))
        ]
        self.assertEqual(
            [statement["Sid"] for statement in denies], ["DenyMissingMarkerTag", "DenyMissingRunTag"]
        )
        for statement in denies:
            with self.subTest(sid=statement["Sid"]):
                keys = [key for block in statement["Condition"].values() for key in block]
                self.assertEqual(len(keys), 1, "a mandatory-tag deny must guard exactly one tag key")

    def test_only_the_describe_and_deny_statements_are_unscoped(self) -> None:
        for sid, statement in self.statements.items():
            if sid in {"DescribeCiState"} or statement["Effect"] == "Deny":
                continue
            resources = statement["Resource"]
            resources = [resources] if isinstance(resources, str) else resources
            for resource in resources:
                with self.subTest(sid=sid, resource=resource):
                    self.assertTrue(resource.startswith(f"arn:aws:ec2:{CI_REGION}:{ACCOUNT_ID}:"))
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
            "repo:nunocgoncalves/iterabase-mono:*",
        )
        self.assertNotIn("environment", json.dumps(document))


class LaunchSurfaceTests(unittest.TestCase):
    def command(self) -> list[str]:
        return launch_command(
            region=CI_REGION,
            image_id="ami-0123456789abcdef0",
            instance_type=APPROVED_INSTANCE_TYPES["cpu"],
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

    def test_workflows_are_dispatch_only_and_use_the_repository_variables(self) -> None:
        smoke = (ROOT / ".github/workflows/aws-ci-smoke.yml").read_text(encoding="utf-8")
        reaper = (ROOT / ".github/workflows/aws-ci-reaper.yml").read_text(encoding="utf-8")
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
            "aws-ci-reaper.yml",
        ):
            with self.subTest(expected=expected):
                self.assertIn(expected, runbook)


if __name__ == "__main__":
    unittest.main()
