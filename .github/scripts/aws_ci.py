#!/usr/bin/env python3
"""AWS CI fixture-account contract, smoke validation, and host reaper.

Authority: `DES-HOR-591-01` (HOR-591, tags/IAM boundary), `DES-HOR-591-02` (HOR-591,
region set and GPU type allowlist) and `docs/runbooks/aws-ci.md`.

This module is the single implementation of the AWS CI substrate contract: the
mandatory tag scheme, the least-privilege CI role policy, the dispatchable smoke
validation, and the scheduled host reaper. It talks to AWS only through the
`aws` CLI so the identical code runs from GitHub Actions and from an operator
shell, and the pure decision functions stay unit-testable without credentials.
"""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile
import time
from typing import Any

# Regions the substrate may use, in preference order. eu-west-1 is primary: it holds
# the default VPC, the bootstrap AMI and the cheapest g5 GPU when it has capacity.
# The others exist so a GPU fixture is never unavailable because one region is dry
# (eu-west-1 offers g5 but has no capacity, and the g6/L4 family is not offered there
# at all, so a single region cannot satisfy "always have a GPU").
CI_REGIONS = ("eu-west-1", "eu-central-1", "eu-north-1")
CI_REGION = CI_REGIONS[0]

# Approved instance types per capacity. GPU types are ordered cheapest-first and all
# provide >= sm_86 with 24 GiB (g6 = L4/sm_89, g5 = A10G/sm_86), which the validated
# inference stack requires; the launch loop tries each type and each offered AZ.
APPROVED_INSTANCE_TYPES: dict[str, tuple[str, ...]] = {
    "cpu": ("m6i.xlarge",),
    "gpu": ("g6.xlarge", "g5.xlarge", "g6.2xlarge", "g5.2xlarge", "g5.4xlarge"),
}
ALL_APPROVED_INSTANCE_TYPES: tuple[str, ...] = tuple(
    dict.fromkeys(instance_type for types in APPROVED_INSTANCE_TYPES.values() for instance_type in types)
)


def approved_instance_types(capacity: str) -> tuple[str, ...]:
    """Approved types for one capacity, cheapest-first for the GPU family.

    The launch search reads the tuple through this helper so the ordering and the
    allowlist stay single-sourced.
    """
    if capacity not in APPROVED_INSTANCE_TYPES:
        raise AwsCiError(f"capacity must be one of {sorted(APPROVED_INSTANCE_TYPES)}")
    return APPROVED_INSTANCE_TYPES[capacity]


def cpu_instance_type() -> str:
    """The single approved CPU type (a tuple entry, so callers never pass the tuple)."""
    return approved_instance_types("cpu")[0]


def gpu_instance_types() -> tuple[str, ...]:
    """Approved GPU types in the documented cheapest-first order."""
    return approved_instance_types("gpu")
SECURITY_GROUP_NAME = "iterabase-ci-ssh"
MARKER_TAG = "iterabase-ci"
MARKER_VALUE = "true"
RUN_TAG = "iterabase-ci-run"
SCENARIO_TAG = "iterabase-ci-scenario"
DEADLINE_TAG = "iterabase-ci-deadline"
NAME_TAG = "Name"
TAG_PREFIX = "iterabase-ci"
CANONICAL_OWNER = "099720109477"
# GitHub issues immutable subject claims for repositories created after 2026-07-15,
# so the OIDC sub claim pins these IDs: `repo:OWNER@OWNER_ID/REPO@REPO_ID:*`.
# They are immutable, so a rename or a recreated repository cannot inherit trust.
GITHUB_OWNER_ID = "64640406"
GITHUB_REPOSITORY_ID = "1330311216"
UBUNTU_IMAGE_NAME = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"
SSH_USER = "ubuntu"
DATA_VOLUME_DEVICE = "/dev/sdf"
# EC2 returns the block-device name as requested, or in its Xen-compatible form.
DATA_VOLUME_DEVICE_NAMES = ("/dev/sdf", "/dev/xvdf")
DATA_VOLUME_GIB = 8
DEFAULT_MAX_AGE_MINUTES = 180
SSH_POLL_SECONDS = 15
SSH_TIMEOUT_SECONDS = 900
TERMINATION_TIMEOUT_SECONDS = 600
MAX_POLICY_CHARACTERS = 6144
HOST_KEY_DELIMITER = "ITERABASE_CI_HOST_KEY"
HOST_KEY_PUB_DELIMITER = "ITERABASE_CI_HOST_KEY_PUB"
AUTHORIZED_KEY_DELIMITER = "ITERABASE_CI_AUTHORIZED_KEY"
SSHD_DELIMITER = "ITERABASE_CI_SSHD"
SSHD_CONFIG_PATH = "/etc/ssh/sshd_config.d/60-iterabase-ci.conf"

TAG_KEYS = (f"{TAG_PREFIX}*", NAME_TAG)
TAG_CREATE_ACTIONS = ["RunInstances", "CreateVolume", "CreateImage", "CopyImage", "RegisterImage", "CreateSnapshot"]
DESCRIBE_ACTIONS = [
    "ec2:DescribeInstances",
    "ec2:DescribeImages",
    "ec2:DescribeVolumes",
    "ec2:DescribeSnapshots",
    "ec2:DescribeInstanceAttribute",
    "ec2:DescribeInstanceTypeOfferings",
    "ec2:DescribeAvailabilityZones",
    "ec2:DescribeVpcs",
    "ec2:DescribeSubnets",
    "ec2:DescribeSecurityGroups",
]


class AwsCiError(RuntimeError):
    """A failing fail-closed condition in the AWS CI contract or its execution."""


# --------------------------------------------------------------------------- #
# Pure contract functions
# --------------------------------------------------------------------------- #


def required_tags(run_id: str, scenario: str, *, deadline: dt.datetime | None = None) -> list[dict[str, str]]:
    """Return the mandatory creation tags for one CI resource (DES-HOR-591-01)."""
    for name, value in (("run id", run_id), ("scenario", scenario)):
        if not value or any(character.isspace() for character in value):
            raise AwsCiError(f"CI {name} must be a non-empty token without whitespace")
    tags = [
        {"Key": MARKER_TAG, "Value": MARKER_VALUE},
        {"Key": RUN_TAG, "Value": run_id},
        {"Key": SCENARIO_TAG, "Value": scenario},
        {"Key": NAME_TAG, "Value": f"{TAG_PREFIX}-{run_id}-{scenario}"},
    ]
    if deadline is not None:
        tags.append({"Key": DEADLINE_TAG, "Value": format_timestamp(deadline)})
    return tags


def tag_specifications(resource_type: str, tags: list[dict[str, str]]) -> str:
    entries = ",".join(f"{{Key={tag['Key']},Value={tag['Value']}}}" for tag in tags)
    return f"ResourceType={resource_type},Tags=[{entries}]"


def format_timestamp(value: dt.datetime) -> str:
    return value.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_timestamp(value: str) -> dt.datetime:
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise AwsCiError(f"invalid timestamp {value!r}") from exc
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
    return parsed.astimezone(dt.timezone.utc)


def by_id_glob(volume_id: str) -> str:
    """Return the by-id symlink glob that EBS volume identity resolves through."""
    if not re.fullmatch(r"vol-[0-9a-f]{8,17}", volume_id):
        raise AwsCiError(f"EBS volume id is not canonical: {volume_id!r}")
    return f"/dev/disk/by-id/*-Amazon_Elastic_Block_Store_{volume_id.replace('-', '')}"


def openssh_sha256_fingerprint(public_key: str) -> str:
    """Return the OpenSSH `SHA256:...` fingerprint of a one-line public key."""
    fields = public_key.split()
    if len(fields) < 2:
        raise AwsCiError(f"public key is not a one-line OpenSSH key: {public_key!r}")
    try:
        blob = base64.b64decode(fields[1], validate=True)
    except ValueError as exc:
        raise AwsCiError("public key payload is not base64") from exc
    digest = base64.b64encode(hashlib.sha256(blob).digest()).decode().rstrip("=")
    return f"SHA256:{digest}"


def host_trust_entry(address: str, public_key: str) -> str:
    """Return the pinned known_hosts line for a host address and public key."""
    fields = public_key.split()
    if len(fields) < 2:
        raise AwsCiError(f"public key is not a one-line OpenSSH key: {public_key!r}")
    if not address or any(character.isspace() for character in address):
        raise AwsCiError(f"host address is not a token: {address!r}")
    # OpenSSH only consults a bracketed `[host]:port` entry for a non-default port
    # (verified: `[github.com]:22` produced "No ED25519 host key is known" while the
    # bare form matched), and the fixture listens on the default port, so the entry
    # uses the bare host form.
    return f"{address} {fields[0]} {fields[1]}"


def render_user_data(*, host_private_key: str, host_public_key: str, authorized_key: str) -> str:
    """Render the cloud-init user-data that installs the per-run SSH identities.

    The host private key is injected here and pinned out of band by the caller,
    which is the HOR-521 verified-host-trust contract in its per-run form.
    """
    private_key = host_private_key.strip("\n")
    public_key = host_public_key.strip("\n")
    authorized = authorized_key.strip("\n")
    if "PRIVATE KEY" not in private_key:
        raise AwsCiError("host private key is not OpenSSH PEM material")
    if len(authorized.split()) < 2 or not public_key.split():
        raise AwsCiError("authorized key and host public key must be one-line OpenSSH keys")
    for name, payload, delimiter in (
        ("host private key", private_key, HOST_KEY_DELIMITER),
        ("host public key", public_key, HOST_KEY_PUB_DELIMITER),
        ("authorized key", authorized, AUTHORIZED_KEY_DELIMITER),
    ):
        if any(line.strip() == delimiter for line in payload.splitlines()):
            raise AwsCiError(f"{name} contains the heredoc delimiter {delimiter}")
    return f"""#!/bin/bash
# Rendered by .github/scripts/aws_ci.py (DES-HOR-591-01); do not edit on the host.
set -euo pipefail

install -d -m 0700 -o {SSH_USER} -g {SSH_USER} /home/{SSH_USER}/.ssh

umask 077
cat > /etc/ssh/ssh_host_ed25519_key <<'{HOST_KEY_DELIMITER}'
{private_key}
{HOST_KEY_DELIMITER}
cat > /etc/ssh/ssh_host_ed25519_key.pub <<'{HOST_KEY_PUB_DELIMITER}'
{public_key}
{HOST_KEY_PUB_DELIMITER}
cat > /home/{SSH_USER}/.ssh/authorized_keys <<'{AUTHORIZED_KEY_DELIMITER}'
{authorized}
{AUTHORIZED_KEY_DELIMITER}
umask 022

chmod 0600 /etc/ssh/ssh_host_ed25519_key
chmod 0644 /etc/ssh/ssh_host_ed25519_key.pub
chmod 0600 /home/{SSH_USER}/.ssh/authorized_keys
chown {SSH_USER}:{SSH_USER} /home/{SSH_USER}/.ssh/authorized_keys

rm -f /etc/ssh/ssh_host_ecdsa_key /etc/ssh/ssh_host_ecdsa_key.pub \\
      /etc/ssh/ssh_host_rsa_key /etc/ssh/ssh_host_rsa_key.pub \\
      /etc/ssh/ssh_host_dsa_key /etc/ssh/ssh_host_dsa_key.pub

cat > {SSHD_CONFIG_PATH} <<'{SSHD_DELIMITER}'
HostKey /etc/ssh/ssh_host_ed25519_key
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
PubkeyAuthentication yes
{SSHD_DELIMITER}

systemctl restart ssh
"""


def policy_document(account_id: str, region: str = CI_REGION) -> dict[str, Any]:
    """Render the least-privilege CI role policy for one account and region.

    The rendered document is the durable security boundary described by
    DES-HOR-591-01: approved instance types, CI-owned AMIs, no instance profile,
    mandatory tags, tag-scoped lifecycle actions, and an explicit denial of the
    privilege-escalation and data surface the role must never reach.
    """
    if not re.fullmatch(r"\d{12}", account_id):
        raise AwsCiError(f"account id is not a 12-digit identifier: {account_id!r}")
    if region not in CI_REGIONS:
        raise AwsCiError(f"CI region must be one of {CI_REGIONS}, not {region!r}")

    # ARNs carry a wildcard region: the service spans three regions, and listing each
    # one would push the document past the 6144-character managed-policy limit. The
    # boundary that matters is unchanged (approved types, CI-owned AMIs, no instance
    # profile, mandatory tags); the region set is a service contract, and cost abuse
    # in another region is bounded by the budget alarm.
    def arn(resource: str) -> str:
        return f"arn:aws:ec2:*:{account_id}:{resource}/*"

    def public_arn(resource: str) -> str:
        return f"arn:aws:ec2:*::{resource}/*"

    def request_tag_condition(
        *,
        extra_string_equals: dict[str, Any] | None = None,
        require_no_instance_profile: bool = False,
    ) -> dict[str, Any]:
        nulls: dict[str, str] = {"aws:RequestTag/" + RUN_TAG: "false"}
        if require_no_instance_profile:
            nulls["ec2:InstanceProfile"] = "true"
        return {
            "StringEquals": {
                "aws:RequestTag/" + MARKER_TAG: MARKER_VALUE,
                **(extra_string_equals or {}),
            },
            "Null": nulls,
            "ForAllValues:StringLike": {"aws:TagKeys": list(TAG_KEYS)},
        }

    marker_condition = {"StringEquals": {"ec2:ResourceTag/" + MARKER_TAG: MARKER_VALUE}}
    statements: list[dict[str, Any]] = [
        {
            "Sid": "DescribeCiState",
            "Effect": "Allow",
            "Action": DESCRIBE_ACTIONS,
            "Resource": "*",
        },
        {
            "Sid": "LaunchFromCiOwnedAmi",
            "Effect": "Allow",
            "Action": "ec2:RunInstances",
            # EC2 reports a copied AMI's ARN with an empty account segment
            # (`arn:aws:ec2:<region>::image/ami-...`, decoded from a real launch
            # denial), so both ARN forms are allowed and `ec2:Owner` keeps the allow
            # to AMIs this account owns.
            "Resource": [public_arn("image"), arn("image")],
            "Condition": {"StringEquals": {"ec2:Owner": account_id}},
        },
        {
            "Sid": "RunApprovedInstances",
            "Effect": "Allow",
            "Action": "ec2:RunInstances",
            # AWS's documented way to require tags at launch: the tag mandate lives on
            # the instance (and volume) resources, whose context carries
            # aws:RequestTag, while the plumbing resources stay condition-free —
            # conditions are evaluated per resource, and ec2:InstanceType exists only
            # on the instance context. A launch with no tags, or without the marker and
            # run tags, fails here even though ec2:CreateTags is what performs the
            # tagging.
            "Resource": arn("instance"),
            "Condition": {
                "StringEquals": {
                    "ec2:InstanceType": list(ALL_APPROVED_INSTANCE_TYPES),
                    "aws:RequestTag/" + MARKER_TAG: MARKER_VALUE,
                },
                "Null": {"ec2:InstanceProfile": "true", "aws:RequestTag/" + RUN_TAG: "false"},
                "ForAllValues:StringLike": {"aws:TagKeys": list(TAG_KEYS)},
            },
        },
        {
            "Sid": "RunApprovedVolumes",
            "Effect": "Allow",
            "Action": "ec2:RunInstances",
            "Resource": arn("volume"),
            "Condition": request_tag_condition(),
        },
        {
            "Sid": "RunInstanceDependencies",
            "Effect": "Allow",
            "Action": "ec2:RunInstances",
            "Resource": [arn("network-interface"), arn("subnet"), arn("key-pair")],
        },
        {
            "Sid": "UseOnlyCiSecurityGroup",
            "Effect": "Allow",
            "Action": "ec2:RunInstances",
            "Resource": arn("security-group"),
            "Condition": marker_condition,
        },
        {
            "Sid": "TagCiResourcesOnCreate",
            "Effect": "Allow",
            "Action": "ec2:CreateTags",
            "Resource": [
                arn("instance"),
                arn("volume"),
                arn("network-interface"),
                arn("image"),
                arn("snapshot"),
            ],
            "Condition": request_tag_condition(
                extra_string_equals={"ec2:CreateAction": TAG_CREATE_ACTIONS}
            ),
        },
        {
            # A copy's tag-on-create is evaluated against wildcard image and snapshot
            # ARNs whose account segment is empty (verified by decoding
            # UnauthorizedOperation on ec2:CreateTags for a copy), so the
            # account-scoped statement above cannot match it. Tagging a foreign
            # resource is impossible regardless, and the same request-tag conditions
            # still require the mandatory tags.
            "Sid": "TagCopiedImagesOnCreate",
            "Effect": "Allow",
            "Action": "ec2:CreateTags",
            "Resource": [public_arn("image"), public_arn("snapshot")],
            "Condition": request_tag_condition(extra_string_equals={"ec2:CreateAction": ["CopyImage"]}),
        },
        {
            "Sid": "TerminateCiInstances",
            "Effect": "Allow",
            "Action": "ec2:TerminateInstances",
            "Resource": arn("instance"),
            "Condition": marker_condition,
        },
        {
            "Sid": "CreateCiVolumes",
            "Effect": "Allow",
            "Action": "ec2:CreateVolume",
            "Resource": arn("volume"),
            "Condition": request_tag_condition(),
        },
        {
            "Sid": "ManageCiVolumes",
            "Effect": "Allow",
            "Action": ["ec2:DeleteVolume", "ec2:AttachVolume", "ec2:DetachVolume"],
            "Resource": arn("volume"),
            "Condition": marker_condition,
        },
        {
            "Sid": "AttachVolumesToCiInstances",
            "Effect": "Allow",
            "Action": ["ec2:AttachVolume", "ec2:DetachVolume"],
            "Resource": arn("instance"),
            "Condition": marker_condition,
        },
        {
            "Sid": "BuildCiImagesAndSnapshots",
            "Effect": "Allow",
            "Action": ["ec2:CreateImage", "ec2:RegisterImage"],
            "Resource": [arn("image"), arn("snapshot")],
            "Condition": request_tag_condition(),
        },
        {
            # EC2 authorizes a copy against two image resources: the source image
            # ARN (a public image is an empty-account ARN, so the account-scoped
            # pattern above cannot match it) and a destination wildcard ARN whose
            # authorization context carries no aws:RequestTag keys at all (verified
            # by decoding an UnauthorizedOperation on CopyImage). A tag condition
            # here would therefore silently deny every copy, so this statement is
            # unconditional; a copy always lands in this account, and the copied
            # AMI's mandatory tags are enforced by TagCiResourcesOnCreate, whose
            # ec2:CreateAction list includes CopyImage.
            "Sid": "CopyImagesIntoTheCiAccount",
            "Effect": "Allow",
            "Action": "ec2:CopyImage",
            "Resource": [
                public_arn("image"),
                public_arn("snapshot"),
                arn("image"),
                arn("snapshot"),
            ],
            # Neither `aws:RequestTag` (absent from both copy contexts) nor the image
            # attribute keys (absent from the destination context) can discriminate
            # here, but `ec2:Owner` is present in both: it is `amazon` for the public
            # Canonical source and this account for the destination wildcard, so this
            # pins the source to Amazon/Canonical images and still lets the copy land.
            "Condition": {"StringEquals": {"ec2:Owner": ["amazon", account_id]}},
        },
        {
            "Sid": "CreateCiSnapshots",
            "Effect": "Allow",
            "Action": "ec2:CreateSnapshot",
            "Resource": [arn("snapshot"), arn("volume")],
            "Condition": request_tag_condition(),
        },
        {
            "Sid": "RemoveCiImagesAndSnapshots",
            "Effect": "Allow",
            "Action": ["ec2:DeregisterImage", "ec2:DeleteSnapshot"],
            # EC2 reports a copied image's own ARN with an empty account segment
            # (`arn:aws:ec2:<region>::image/ami-...`, proven by decoding
            # DeregisterImage of the CI-owned bootstrap copy while its
            # ec2:ResourceTag keys were present), so both account forms are allowed
            # and the tag condition still scopes the action to CI resources.
            "Resource": [
                public_arn("image"),
                public_arn("snapshot"),
                arn("image"),
                arn("snapshot"),
            ],
            "Condition": marker_condition,
        },
        {
            "Sid": "DenyInstanceProfile",
            "Effect": "Deny",
            "Action": "ec2:RunInstances",
            "Resource": "*",
            "Condition": {"Null": {"ec2:InstanceProfile": "false"}},
        },
        {
            # Scope every context-key deny to the resource whose authorization
            # context actually carries the key: a `StringNotEquals` on an absent key
            # evaluates true, so an unscoped deny here blocked every launch (proven
            # by decoding RunInstances' `aws:ResourceBeingCreated` context, which has
            # ec2:InstanceType but no ec2:Owner).
            "Sid": "DenyUnapprovedInstanceType",
            "Effect": "Deny",
            "Action": "ec2:RunInstances",
            "Resource": arn("instance"),
            "Condition": {"StringNotEquals": {"ec2:InstanceType": list(ALL_APPROVED_INSTANCE_TYPES)}},
        },
        {
            "Sid": "DenyNonCiOwnedAmi",
            "Effect": "Deny",
            "Action": "ec2:RunInstances",
            "Resource": [public_arn("image"), arn("image")],
            "Condition": {"StringNotEquals": {"ec2:Owner": account_id}},
        },
        {
            # Keep the region boundary in the artifact AWS enforces rather than in the
            # driver: `aws:RequestedRegion` is a global request key, so one small deny
            # replaces expanding every ARN three ways (9,136 characters, above the
            # 6,144 limit).
            "Sid": "DenyOutsideCiRegions",
            "Effect": "Deny",
            "Action": "ec2:*",
            "Resource": "*",
            "Condition": {"StringNotEquals": {"aws:RequestedRegion": list(CI_REGIONS)}},
        },
        {
            "Sid": "DenyPrivilegeAndDataSurface",
            "Effect": "Deny",
            "Action": ["iam:*", "organizations:*", "s3:*", "ssm:*", "sts:AssumeRole"],
            "Resource": "*",
        },
    ]
    document = {"Version": "2012-10-17", "Statement": statements}
    length = len(json.dumps(document, separators=(",", ":")))
    if length > MAX_POLICY_CHARACTERS:
        raise AwsCiError(f"rendered policy is {length} characters, above the {MAX_POLICY_CHARACTERS} limit")
    return document


def render_role_trust_policy(
    account_id: str = "",
    repository: str = "nunocgoncalves/iterabase-mono",
    owner_id: str = GITHUB_OWNER_ID,
    repository_id: str = GITHUB_REPOSITORY_ID,
) -> dict[str, Any]:
    """Render the GitHub OIDC trust policy (no environment, no static keys).

    The provider ARN carries the management-account id supplied by the operator;
    the placeholder is only used to show the shape before the account exists.
    GitHub emits immutable subject claims for this repository, so the sub claim
    pins the owner and repository IDs instead of matching the classic name form.
    """
    if not re.fullmatch(r"[^/\s]+/[^/\s]+", repository):
        raise AwsCiError(f"repository is not owner/name: {repository!r}")
    if account_id and not re.fullmatch(r"\d{12}", account_id):
        raise AwsCiError(f"account id is not a 12-digit identifier: {account_id!r}")
    for name, value in (("owner id", owner_id), ("repository id", repository_id)):
        if not re.fullmatch(r"\d+", str(value)):
            raise AwsCiError(f"GitHub {name} is not numeric: {value!r}")
    owner, repository_name = repository.split("/", 1)
    subject = f"repo:{owner}@{owner_id}/{repository_name}@{repository_id}:*"
    provider_account = account_id or "ACCOUNT_ID"
    return {
        "Version": "2012-10-17",
        "Statement": [
            {
                "Sid": "GitHubOidcRepository",
                "Effect": "Allow",
                "Principal": {
                    "Federated": f"arn:aws:iam::{provider_account}:oidc-provider/token.actions.githubusercontent.com"
                },
                "Action": "sts:AssumeRoleWithWebIdentity",
                "Condition": {
                    "StringEquals": {"token.actions.githubusercontent.com:aud": "sts.amazonaws.com"},
                    "StringLike": {"token.actions.githubusercontent.com:sub": subject},
                },
            }
        ],
    }


def launch_command(
    *,
    region: str,
    image_id: str,
    instance_type: str,
    subnet_id: str,
    security_group_id: str,
    tags: list[dict[str, str]],
    user_data_path: str,
) -> list[str]:
    """Build the exact approved-instance launch command (never with a profile)."""
    block_device = (
        f"DeviceName={DATA_VOLUME_DEVICE},"
        f"Ebs={{VolumeSize={DATA_VOLUME_GIB},VolumeType=gp3,DeleteOnTermination=true}}"
    )
    return [
        "aws",
        "ec2",
        "run-instances",
        "--region",
        region,
        "--image-id",
        image_id,
        "--instance-type",
        instance_type,
        "--count",
        "1",
        "--subnet-id",
        subnet_id,
        "--security-group-ids",
        security_group_id,
        "--associate-public-ip-address",
        "--instance-initiated-shutdown-behavior",
        "terminate",
        "--metadata-options",
        "HttpTokens=required,HttpEndpoint=enabled",
        "--block-device-mappings",
        block_device,
        "--tag-specifications",
        tag_specifications("instance", tags),
        tag_specifications("volume", tags),
        "--user-data",
        f"file://{user_data_path}",
        "--output",
        "json",
        "--query",
        "Instances[0].InstanceId",
    ]


def ssh_command(
    *,
    key_path: str,
    known_hosts_path: str,
    address: str,
    remote_command: str,
) -> list[str]:
    """Build a strictly pinned SSH invocation for one remote command."""
    return [
        "ssh",
        "-i",
        key_path,
        "-o",
        "IdentitiesOnly=yes",
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=10",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        "CheckHostIP=no",
        "-o",
        f"UserKnownHostsFile={known_hosts_path}",
        f"{SSH_USER}@{address}",
        remote_command,
    ]


def device_probe_command(volume_id: str) -> str:
    """Build the remote probe that proves EBS by-id identity from a volume id."""
    return (
        "set -euo pipefail\n"
        "shopt -s nullglob\n"
        f"matches=({by_id_glob(volume_id)})\n"
        'if [ "${#matches[@]}" -ne 1 ]; then\n'
        '  echo "expected exactly one by-id symlink for the data volume, found ${#matches[@]}" >&2\n'
        "  exit 1\n"
        "fi\n"
        'device="$(readlink -f "${matches[0]}")"\n'
        'test -b "$device"\n'
        'echo "by_id=${matches[0]}"\n'
        'echo "device=$device"\n'
        'echo "size_bytes=$(lsblk -b -d -n -o SIZE "$device")"\n'
    )


def identity_probe_command() -> str:
    """Build the remote probe that proves only the per-run host identity is live."""
    return (
        "set -euo pipefail\n"
        "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | awk '{print $2}'\n"
        "test ! -e /etc/ssh/ssh_host_rsa_key\n"
        "test ! -e /etc/ssh/ssh_host_ecdsa_key\n"
        "test ! -e /etc/ssh/ssh_host_dsa_key\n"
        f"test -f {SSHD_CONFIG_PATH}\n"
        "echo per-run-identity\n"
    )


def verify_required_tags(tags: list[dict[str, str]] | None, *, run_id: str, scenario: str) -> str:
    """Assert a launched instance carries exactly the mandatory CI tags.

    Returns the tag set as `key=value` pairs for the run summary, so the positive
    launch evidence shows the tags the acceptance criterion asks for.
    """
    mandatory = required_tags(run_id, scenario)
    present = {str(tag.get("Key")): str(tag.get("Value")) for tag in tags or []}
    for tag in mandatory:
        if present.get(tag["Key"]) != tag["Value"]:
            raise AwsCiError(
                f"instance is missing the mandatory tag {tag['Key']}={tag['Value']}; "
                f"found {sorted(present)}"
            )
    return ",".join(f"{tag['Key']}={tag['Value']}" for tag in mandatory)


def instance_marker(tags: list[dict[str, str]] | None) -> str | None:
    for tag in tags or []:
        if tag.get("Key") == MARKER_TAG:
            return tag.get("Value")
    return None


def tagged_value(tags: list[dict[str, str]] | None, key: str) -> str | None:
    for tag in tags or []:
        if tag.get("Key") == key:
            return tag.get("Value")
    return None


def instance_deadline(instance: dict[str, Any], max_age_minutes: int) -> dt.datetime:
    """Return the termination deadline for one tag-marked CI instance."""
    tags = instance.get("Tags")
    raw_deadline = tagged_value(tags, DEADLINE_TAG)
    if raw_deadline:
        return parse_timestamp(raw_deadline)
    launch_time = instance.get("LaunchTime")
    if not launch_time:
        raise AwsCiError(f"instance {instance.get('InstanceId')} has no launch time")
    return parse_timestamp(str(launch_time)) + dt.timedelta(minutes=max_age_minutes)


def reap_plan(
    instances: list[dict[str, Any]], *, now: dt.datetime, max_age_minutes: int
) -> dict[str, Any]:
    """Split account instances into due, pending, foreign, and invalid decisions."""
    plan: dict[str, Any] = {"terminate": [], "pending": [], "foreign": [], "errors": []}
    for instance in instances:
        instance_id = instance.get("InstanceId") or "unknown"
        if instance_marker(instance.get("Tags")) != MARKER_VALUE:
            plan["foreign"].append(
                {
                    "instance_id": instance_id,
                    "state": (instance.get("State") or {}).get("Name", "unknown"),
                    "name": tagged_value(instance.get("Tags"), NAME_TAG) or "-",
                }
            )
            continue
        try:
            deadline = instance_deadline(instance, max_age_minutes)
        except AwsCiError as exc:
            plan["errors"].append(f"{instance_id}: {exc}")
            continue
        decision = {
            "instance_id": instance_id,
            "state": (instance.get("State") or {}).get("Name", "unknown"),
            "name": tagged_value(instance.get("Tags"), NAME_TAG) or "-",
            "deadline": format_timestamp(deadline),
        }
        if now >= deadline:
            decision["reason"] = (
                f"{DEADLINE_TAG} passed" if tagged_value(instance.get("Tags"), DEADLINE_TAG) else "max age reached"
            )
            plan["terminate"].append(decision)
        else:
            decision["reason"] = "not yet due"
            plan["pending"].append(decision)
    return plan


def denied_launch_cases(
    *,
    image_id: str,
    public_ami_id: str,
    instance_profile: str,
) -> list[dict[str, Any]]:
    """Return the denied launch cases that must fail closed with access-denied."""
    approved = cpu_instance_type()
    return [
        {
            "name": "non-approved-instance-type",
            "detail": "m6i.2xlarge is not an approved CI instance type",
            "image_id": image_id,
            "instance_type": "m6i.2xlarge",
            "tags": "required",
            "instance_profile": None,
        },
        {
            "name": "missing-all-mandatory-tags",
            "detail": "no creation tags at all",
            "image_id": image_id,
            "instance_type": approved,
            "tags": "none",
            "instance_profile": None,
        },
        {
            "name": "missing-run-tag",
            "detail": "marker tag present without the run and scenario tags",
            "image_id": image_id,
            "instance_type": approved,
            "tags": "marker-only",
            "instance_profile": None,
        },
        {
            "name": "non-ci-account-ami",
            "detail": f"public Canonical AMI {public_ami_id} is not owned by the CI account",
            "image_id": public_ami_id,
            "instance_type": approved,
            "tags": "required",
            "instance_profile": None,
        },
        {
            "name": "instance-profile-attached",
            "detail": f"launch references instance profile {instance_profile}",
            "image_id": image_id,
            "instance_type": approved,
            "tags": "required",
            "instance_profile": instance_profile,
        },
    ]


def denied_case_command(
    case: dict[str, Any],
    *,
    region: str,
    run_id: str,
    subnet_id: str,
    security_group_id: str,
) -> list[str]:
    tags = {
        "required": required_tags(run_id, f"denied-{case['name']}"),
        "marker-only": [{"Key": MARKER_TAG, "Value": MARKER_VALUE}],
        "none": [],
    }[case["tags"]]
    command = [
        "aws",
        "ec2",
        "run-instances",
        "--region",
        region,
        "--image-id",
        case["image_id"],
        "--instance-type",
        case["instance_type"],
        "--count",
        "1",
        "--subnet-id",
        subnet_id,
        "--security-group-ids",
        security_group_id,
        "--associate-public-ip-address",
        "--instance-initiated-shutdown-behavior",
        "terminate",
        "--output",
        "json",
    ]
    if tags:
        command += ["--tag-specifications", tag_specifications("instance", tags)]
    if case["instance_profile"]:
        command += ["--iam-instance-profile", f"Name={case['instance_profile']}"]
    return command


def access_denied_action(stderr: str) -> str | None:
    """Return the denied action quoted by an AWS access-denied error, if any."""
    match = re.search(r"not authorized to perform: ([A-Za-z0-9:_-]+)", stderr)
    return match.group(1) if match else None


def error_class(stderr: str) -> str:
    """Return the AWS error class of a failed CLI call, for denial evidence."""
    match = re.search(r"An error occurred \(([A-Za-z0-9]+)\)", stderr)
    return match.group(1) if match else "unknown"


# --------------------------------------------------------------------------- #
# AWS CLI plumbing
# --------------------------------------------------------------------------- #


def aws(args: list[str], *, check: bool = True) -> subprocess.CompletedProcess[str]:
    try:
        completed = subprocess.run(
            ["aws", *args],
            check=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
    except OSError as exc:
        raise AwsCiError(f"cannot run the aws CLI: {exc}") from exc
    if check and completed.returncode != 0:
        raise AwsCiError(f"aws {' '.join(args[:3])} failed:\n{completed.stderr.strip()}")
    return completed


def aws_json(args: list[str]) -> Any:
    completed = aws(args + ["--output", "json"])
    try:
        return json.loads(completed.stdout or "null")
    except json.JSONDecodeError as exc:
        raise AwsCiError(f"aws {' '.join(args[:3])} returned invalid JSON: {exc}") from exc


def require_region(region: str | None) -> str:
    """Validate and return one allowed CI region."""
    resolved = region or os.environ.get("AWS_REGION") or os.environ.get("AWS_CI_REGION") or ""
    if resolved not in CI_REGIONS:
        raise AwsCiError(f"AWS CI region must be one of {CI_REGIONS}, not {resolved or '<unset>'!r}")
    return resolved


def region_order(primary: str | None) -> tuple[str, ...]:
    """Return the regions to search: the primary first, then the rest in order.

    The primary always leads, so a fixture prefers its home region (default VPC,
    bootstrap AMI, cheapest g5) and only moves when capacity or quota says so:
    GPU capacity in eu-west-1 is intermittent and g6 is not offered there at all.
    """
    resolved = require_region(primary)
    return (resolved, *(region for region in CI_REGIONS if region != resolved))


def write_outputs(values: dict[str, str]) -> None:
    path = os.environ.get("GITHUB_OUTPUT")
    if not path:
        return
    with open(path, "a", encoding="utf-8") as handle:
        for key, value in values.items():
            handle.write(f"{key}={value}\n")


def write_summary(markdown: str) -> None:
    print(markdown)
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not path:
        return
    with open(path, "a", encoding="utf-8") as handle:
        handle.write(markdown.rstrip("\n") + "\n")


def run(command: list[str], *, check: bool = True, timeout: int | None = None) -> subprocess.CompletedProcess[str]:
    try:
        completed = subprocess.run(
            command,
            check=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise AwsCiError(f"cannot run {command[0]}: {exc}") from exc
    if check and completed.returncode != 0:
        raise AwsCiError(f"command failed: {shlex.join(command)}\n{completed.stderr.strip()}")
    return completed


# --------------------------------------------------------------------------- #
# Account discovery
# --------------------------------------------------------------------------- #


def verify_identity(region: str, *, write_evidence: bool = True) -> dict[str, str]:
    identity = aws_json(["sts", "get-caller-identity"])
    if not isinstance(identity, dict) or not identity.get("Arn"):
        raise AwsCiError("sts get-caller-identity returned no identity")
    arn = str(identity["Arn"])
    if ":assumed-role/" not in arn:
        raise AwsCiError(f"the CI workflows must assume a role, got {arn}")
    record = {
        "account": str(identity.get("Account", "")),
        "arn": arn,
        "region": region,
    }
    if write_evidence:
        write_summary(
            "### AWS CI identity\n\n"
            f"- account: `{record['account']}`\n"
            f"- role: `{arn}`\n"
            f"- region: `{region}`\n"
        )
    return record


def as_list(value: Any, *, what: str) -> list[Any]:
    if not isinstance(value, list):
        raise AwsCiError(f"{what} is not a list")
    return value


def default_vpc(region: str) -> str:
    vpcs = as_list(
        aws_json(["ec2", "describe-vpcs", "--region", region, "--filters", "Name=isDefault,Values=true"])["Vpcs"],
        what="default VPCs",
    )
    if len(vpcs) != 1:
        raise AwsCiError(f"expected exactly one default VPC, found {len(vpcs)}")
    return str(vpcs[0]["VpcId"])


def ci_security_group(region: str) -> str:
    groups = as_list(
        aws_json(
            [
                "ec2",
                "describe-security-groups",
                "--region",
                region,
                "--filters",
                f"Name=group-name,Values={SECURITY_GROUP_NAME}",
                f"Name=tag:{MARKER_TAG},Values={MARKER_VALUE}",
            ]
        )["SecurityGroups"],
        what="CI security groups",
    )
    if len(groups) != 1:
        raise AwsCiError(
            f"expected exactly one {SECURITY_GROUP_NAME} security group tagged {MARKER_TAG}={MARKER_VALUE}, "
            f"found {len(groups)}; run docs/runbooks/aws-ci.md"
        )
    return str(groups[0]["GroupId"])


def offered_azs(region: str, instance_type: str) -> list[str]:
    offerings = as_list(
        aws_json(
            [
                "ec2",
                "describe-instance-type-offerings",
                "--region",
                region,
                "--location-type",
                "availability-zone",
                "--filters",
                f"Name=instance-type,Values={instance_type}",
            ]
        )["InstanceTypeOfferings"],
        what="instance type offerings",
    )
    zones = as_list(
        aws_json(["ec2", "describe-availability-zones", "--region", region])["AvailabilityZones"],
        what="availability zones",
    )
    available = {str(zone["ZoneName"]) for zone in zones if zone.get("State") == "available"}
    # An empty result means "this type is not offered here", which the placement
    # search treats as a reason to try the next type or region; callers that need the
    # type (the denied cases) check for emptiness themselves.
    return sorted({str(offering["Location"]) for offering in offerings} & available)


def subnets_by_az(region: str, vpc_id: str, azs: list[str]) -> dict[str, str]:
    subnets = as_list(
        aws_json(["ec2", "describe-subnets", "--region", region, "--filters", f"Name=vpc-id,Values={vpc_id}"])[
            "Subnets"
        ],
        what="subnets",
    )
    result: dict[str, str] = {}
    for subnet in subnets:
        zone = str(subnet.get("AvailabilityZone"))
        if zone not in azs or zone in result:
            continue
        if int(subnet.get("AvailableIpAddressCount", 0)) <= 5:
            continue
        result[zone] = str(subnet["SubnetId"])
    return result


# --------------------------------------------------------------------------- #
# Commands
# --------------------------------------------------------------------------- #


def command_render_policy(args: argparse.Namespace) -> int:
    document = policy_document(args.account_id, args.region or CI_REGION)
    rendered = json.dumps(document, indent=2) + "\n"
    if args.output:
        Path(args.output).write_text(rendered, encoding="utf-8")
        print(f"wrote {args.output}")
    else:
        sys.stdout.write(rendered)
    return 0


def command_render_trust_policy(args: argparse.Namespace) -> int:
    document = render_role_trust_policy(args.account_id, args.repository, args.owner_id, args.repository_id)
    rendered = json.dumps(document, indent=2) + "\n"
    if args.output:
        Path(args.output).write_text(rendered, encoding="utf-8")
        print(f"wrote {args.output}")
    else:
        sys.stdout.write(rendered)
    return 0


def command_verify_identity(args: argparse.Namespace) -> int:
    verify_identity(require_region(args.region))
    return 0


def resolve_canonical_ami(region: str) -> dict[str, str]:
    """Return the newest Canonical Ubuntu 24.04 AMD64 EBS image in one region.

    The single source of the image-selection contract: both the bootstrap copy and
    any operator lookup resolve through this filter set.
    """
    images = as_list(
        aws_json(
            [
                "ec2",
                "describe-images",
                "--region",
                region,
                "--owners",
                CANONICAL_OWNER,
                "--filters",
                f"Name=name,Values={UBUNTU_IMAGE_NAME}",
                "Name=state,Values=available",
                "Name=architecture,Values=x86_64",
                "Name=root-device-type,Values=ebs",
                "Name=virtualization-type,Values=hvm",
            ]
        )["Images"],
        what="images",
    )
    if not images:
        raise AwsCiError("no Canonical Ubuntu 24.04 AMD64 image is available")
    latest = sorted(images, key=lambda image: str(image.get("CreationDate")))[-1]
    return {
        "source_ami_id": str(latest["ImageId"]),
        "source_ami_name": str(latest.get("Name", "")),
        "source_ami_created": str(latest.get("CreationDate", "")),
    }


def command_bootstrap_ami(args: argparse.Namespace) -> int:
    """Copy the tagged CI-owned bootstrap AMI into every allowed region.

    A fixture may only launch from an AMI this account owns, so each region that can
    host a fixture needs its own copy: the GPU search spans regions because capacity
    and even the g6 offering differ between them.
    """
    primary = require_region(args.region)
    tags = required_tags(args.run_id, "bootstrap-ami")
    copies: dict[str, str] = {}
    sources: dict[str, str] = {}
    for region in region_order(primary):
        if args.source_ami_id:
            source = {"source_ami_id": args.source_ami_id, "source_ami_name": "operator override"}
        else:
            source = resolve_canonical_ami(region)
        sources[region] = source["source_ami_id"]
        copied = aws_json(
            [
                "ec2",
                "copy-image",
                "--region",
                region,
                "--source-region",
                region,
                "--source-image-id",
                source["source_ami_id"],
                "--name",
                f"{TAG_PREFIX}-ubuntu-24.04-{args.run_id}",
                "--description",
                f"iterabase CI bootstrap AMI for run {args.run_id} (DES-HOR-591-01, DES-HOR-591-02)",
                "--tag-specifications",
                tag_specifications("image", tags),
                tag_specifications("snapshot", tags),
            ]
        )
        image_id = str(copied["ImageId"])
        copies[region] = image_id
        print(json.dumps({"region": region, "ami_id": image_id, **source}))
    for region, image_id in copies.items():
        wait_for_image(region, image_id)
    rendered_map = ",".join(f"{region}={image_id}" for region, image_id in copies.items())
    write_outputs({"ami_id": copies[primary], "ami_ids": rendered_map, "public_ami_id": sources[primary]})
    write_summary(
        "### Bootstrap AMI\n\n"
        + "\n".join(f"- `{region}`: source `{sources[region]}`, CI-owned copy `{copies[region]}`" for region in copies)
        + "\n- tags: "
        + ", ".join(f"`{tag['Key']}={tag['Value']}`" for tag in tags)
        + "\n"
    )
    return 0


def wait_for_image(region: str, image_id: str, timeout_seconds: int = 900) -> None:
    deadline = time.monotonic() + timeout_seconds
    while True:
        images = as_list(
            aws_json(["ec2", "describe-images", "--region", region, "--image-ids", image_id])["Images"],
            what="images",
        )
        if len(images) == 1 and images[0].get("State") == "available":
            return
        if time.monotonic() > deadline:
            raise AwsCiError(f"AMI {image_id} did not become available within {timeout_seconds}s")
        time.sleep(15)


def parse_ami_ids(value: str, primary: str = CI_REGION) -> dict[str, str]:
    """Parse `region=ami-...,region=ami-...`, or a bare id for `primary`.

    The bare form binds to the *resolved* primary region, not the module constant:
    with `AWS_CI_REGION=eu-central-1`, a bare id mapped to `eu-west-1` would leave the
    launch search with no AMI for the region it actually searches.
    """
    primary = require_region(primary)
    mapping: dict[str, str] = {}
    for entry in value.split(","):
        entry = entry.strip()
        if not entry:
            continue
        if "=" in entry:
            region, ami = (part.strip() for part in entry.split("=", 1))
            if region not in CI_REGIONS:
                raise AwsCiError(f"AMI map names an unknown region: {region!r}")
            mapping[region] = ami
        else:
            mapping[primary] = entry
    if not mapping:
        raise AwsCiError("no AMI ids were supplied")
    return mapping


def place_fixture(
    *,
    capacity: str,
    run_id: str,
    ami_ids: dict[str, str],
    user_data_path: str,
    regions: tuple[str, ...],
    attempts: int = 2,
) -> tuple[str, str, str, str, list[str]]:
    """Find somewhere the fixture can actually launch: region, type, AZ, instance id.

    Capacity decides, not configuration: eu-west-1 offers g5 but has had no live GPU
    capacity, the g6/L4 family is not offered there at all, and another region may
    lack quota. The search therefore walks regions, then approved types, then offered
    AZs, and a pass with only capacity/quota failures waits briefly and runs again.
    """
    scenario = f"smoke-{capacity}"
    failures: list[str] = []
    for attempt in range(1, max(1, attempts) + 1):
        for region in regions:
            ami_id = ami_ids.get(region)
            if not ami_id:
                continue
            vpc = default_vpc(region)
            security_group = ci_security_group(region)
            for instance_type in approved_instance_types(capacity):
                azs = offered_azs(region, instance_type)
                if not azs:
                    continue
                for az, subnet_id in sorted(subnets_by_az(region, vpc, azs).items()):
                    command = launch_command(
                        region=region,
                        image_id=ami_id,
                        instance_type=instance_type,
                        subnet_id=subnet_id,
                        security_group_id=security_group,
                        tags=required_tags(run_id, scenario),
                        user_data_path=user_data_path,
                    )
                    completed = aws(command[1:], check=False)
                    if completed.returncode == 0:
                        return region, instance_type, az, completed.stdout.strip().strip('"'), failures
                    error = error_class(completed.stderr)
                    if "InsufficientInstanceCapacity" in completed.stderr or "VcpuLimitExceeded" in completed.stderr:
                        failures.append(f"{region}/{instance_type}/{az}: {error}")
                        continue
                    raise AwsCiError(
                        f"launching the {capacity} fixture failed in {region}/{az}:\n{completed.stderr.strip()}"
                    )
        if attempt < max(1, attempts):
            time.sleep(60)
    return "", "", "", "", failures


def command_run_host(args: argparse.Namespace) -> int:
    primary = require_region(args.region)
    approved_instance_types(args.capacity)
    ami_ids = parse_ami_ids(args.ami_ids, primary)
    scenario = f"smoke-{args.capacity}"
    # CPU capacity is not scarce, so it stays in the primary region; the GPU family
    # walks the whole allowed region order.
    regions = region_order(primary) if args.capacity == "gpu" else (primary,)
    workdir = Path(tempfile.mkdtemp(prefix=f"iterabase-ci-{args.capacity}-"))
    ssh_key = workdir / "id_ed25519"
    host_key = workdir / "host_ed25519"
    wrong_host_key = workdir / "wrong_host_ed25519"
    for path in (ssh_key, host_key, wrong_host_key):
        run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", f"iterabase-ci-smoke-{args.capacity}", "-f", str(path)])
    host_public = (host_key.with_suffix(".pub")).read_text(encoding="utf-8").strip()
    authorized = (ssh_key.with_suffix(".pub")).read_text(encoding="utf-8").strip()
    user_data = render_user_data(
        host_private_key=(host_key).read_text(encoding="utf-8"),
        host_public_key=host_public,
        authorized_key=authorized,
    )
    user_data_path = workdir / "user-data.sh"
    user_data_path.write_text(user_data, encoding="utf-8")

    region, instance_type, launched_az, instance_id, capacity_failures = place_fixture(
        capacity=args.capacity,
        run_id=args.run_id,
        ami_ids=ami_ids,
        user_data_path=str(user_data_path),
        regions=regions,
    )
    if not instance_id:
        raise AwsCiError(
            "no allowed region, approved type, or offered availability zone could launch the fixture: "
            + "; ".join(capacity_failures or ["no candidates"])
        )

    evidence: dict[str, str] = {
        "capacity": args.capacity,
        "region": region,
        "instance_type": instance_type,
        "instance_id": instance_id,
        "availability_zone": launched_az,
        "ami_id": ami_ids[region],
        "host_key_fingerprint": openssh_sha256_fingerprint(host_public),
    }
    try:
        instance = wait_for_running(region, instance_id)
        public_ip = str(instance.get("PublicIpAddress") or "")
        if not public_ip:
            raise AwsCiError(f"instance {instance_id} has no public address")
        evidence["public_ip"] = public_ip
        evidence["tags"] = verify_required_tags(instance.get("Tags"), run_id=args.run_id, scenario=scenario)
        evidence["shutdown_behavior"] = instance_shutdown_behavior(region, instance_id)
        volume_id = data_volume_id(region, instance_id)
        evidence["data_volume_id"] = volume_id

        known_hosts = workdir / "known_hosts"
        known_hosts.write_text(host_trust_entry(public_ip, host_public) + "\n", encoding="utf-8")
        pinned = {
            "key_path": str(ssh_key),
            "known_hosts_path": str(known_hosts),
            "address": public_ip,
        }
        wait_for_pinned_ssh(pinned)
        identity_probe = run(ssh_command(**pinned, remote_command=identity_probe_command()))
        identity_lines = identity_probe.stdout.strip().splitlines()
        if not identity_lines or identity_lines[0] != evidence["host_key_fingerprint"]:
            raise AwsCiError(
                f"remote host key fingerprint {identity_lines[0] if identity_lines else '<none>'} does not match the pinned "
                f"{evidence['host_key_fingerprint']}"
            )
        if identity_lines[-1] != "per-run-identity":
            raise AwsCiError(f"the fixture kept stock host key material: {identity_probe.stdout.strip()}")
        evidence["remote_host_key_fingerprint"] = identity_lines[0]
        evidence["stock_host_keys"] = "absent"

        wrong_hosts = workdir / "wrong_known_hosts"
        wrong_hosts.write_text(
            host_trust_entry(public_ip, (wrong_host_key.with_suffix(".pub")).read_text(encoding="utf-8")),
            encoding="utf-8",
        )
        mismatch = run(
            ssh_command(
                key_path=str(ssh_key),
                known_hosts_path=str(wrong_hosts),
                address=public_ip,
                remote_command="true",
            ),
            check=False,
        )
        if mismatch.returncode == 0 or "Host key verification failed" not in mismatch.stderr:
            raise AwsCiError(
                "a wrong pinned host key was not rejected at key exchange: "
                f"exit {mismatch.returncode}, {mismatch.stderr.strip() or mismatch.stdout.strip()}"
            )
        evidence["wrong_host_key_rejected"] = "Host key verification failed"

        probe = wait_for_device_probe(pinned, volume_id)
        probe_fields = dict(
            line.split("=", 1) for line in probe.strip().splitlines() if "=" in line
        )
        if not probe_fields.get("by_id", "").endswith(volume_id.replace("-", "")):
            raise AwsCiError(f"by-id device does not match volume {volume_id}: {probe.stdout.strip()}")
        volume = aws_json(["ec2", "describe-volumes", "--region", region, "--volume-ids", volume_id])["Volumes"][0]
        expected_size = int(volume["Size"]) * 1024 * 1024 * 1024
        if int(probe_fields.get("size_bytes", "0")) != expected_size:
            raise AwsCiError(
                f"by-id device size {probe_fields.get('size_bytes')} does not match volume size {expected_size}"
            )
        evidence["by_id_device"] = probe_fields["by_id"]
        evidence["device"] = probe_fields["device"]
        evidence["device_size_bytes"] = probe_fields["size_bytes"]

        run(ssh_command(**pinned, remote_command="sudo shutdown -h now"), check=False)
        terminated = wait_for_termination(region, instance_id)
        evidence["final_state"] = str((terminated.get("State") or {}).get("Name", ""))
        evidence["state_reason"] = str((terminated.get("StateReason") or {}).get("Code", ""))
        if evidence["final_state"] != "terminated":
            raise AwsCiError(f"instance {instance_id} ended in state {evidence['final_state']!r}")
    finally:
        if capacity_failures:
            evidence["insufficient_capacity"] = "; ".join(capacity_failures)
        write_summary(host_summary(evidence))
        write_outputs({key: value for key, value in evidence.items() if key in {"instance_id", "public_ip"}})
    return 0


def host_summary(evidence: dict[str, str]) -> str:
    rows = [
        ("capacity", evidence.get("capacity", "-")),
        ("region", evidence.get("region", "-")),
        ("instance type", evidence.get("instance_type", "-")),
        ("instance id", evidence.get("instance_id", "-")),
        ("mandatory tags", evidence.get("tags", "-")),
        ("availability zone", evidence.get("availability_zone", "-")),
        ("pinned host key", evidence.get("host_key_fingerprint", "-")),
        ("remote host key", evidence.get("remote_host_key_fingerprint", "-")),
        ("stock host keys", evidence.get("stock_host_keys", "-")),
        ("wrong pinned key", evidence.get("wrong_host_key_rejected", "-")),
        ("by-id device", evidence.get("by_id_device", "-")),
        ("device size bytes", evidence.get("device_size_bytes", "-")),
        ("shutdown behavior", evidence.get("shutdown_behavior", "-")),
        ("final state", evidence.get("final_state", "-")),
        ("state reason", evidence.get("state_reason", "-")),
        ("insufficient capacity", evidence.get("insufficient_capacity", "none")),
    ]
    lines = [f"### Smoke host {evidence.get('capacity', '')}".rstrip(), "", "| evidence | value |", "| --- | --- |"]
    lines += [f"| {name} | `{value}` |" for name, value in rows]
    return "\n".join(lines) + "\n"


def instance_shutdown_behavior(region: str, instance_id: str) -> str:
    """Return the instance-initiated shutdown behavior, failing closed on drift.

    `describe-instances` omits the field (verified: it returned nothing while the
    attribute API returned `terminate` for the same instance), so read the attribute.
    """
    payload = aws_json(
        [
            "ec2",
            "describe-instance-attribute",
            "--region",
            region,
            "--instance-id",
            instance_id,
            "--attribute",
            "instanceInitiatedShutdownBehavior",
        ]
    )
    value = str(((payload.get("InstanceInitiatedShutdownBehavior") or {}).get("Value")) or "")
    if value != "terminate":
        raise AwsCiError(f"instance {instance_id} shutdown behavior is {value!r}, expected 'terminate'")
    return value


def wait_for_running(region: str, instance_id: str, timeout_seconds: int = 300) -> dict[str, Any]:
    deadline = time.monotonic() + timeout_seconds
    while True:
        instance = describe_instance(region, instance_id)
        if instance.get("State", {}).get("Name") == "running" and instance.get("PublicIpAddress"):
            return instance
        if time.monotonic() > deadline:
            raise AwsCiError(f"instance {instance_id} did not reach running with a public address")
        time.sleep(10)


def describe_instance(region: str, instance_id: str) -> dict[str, Any]:
    reservations = as_list(
        aws_json(["ec2", "describe-instances", "--region", region, "--instance-ids", instance_id])["Reservations"],
        what="reservations",
    )
    if len(reservations) != 1 or len(reservations[0].get("Instances", [])) != 1:
        raise AwsCiError(f"expected exactly one instance for {instance_id}")
    return reservations[0]["Instances"][0]


def data_volume_id_from_instance(instance: dict[str, Any]) -> str:
    """Return the attached data-volume id from one describe-instances entry."""
    attached = [
        str(mapping["Ebs"]["VolumeId"])
        for mapping in instance.get("BlockDeviceMappings", [])
        if mapping.get("DeviceName") in DATA_VOLUME_DEVICE_NAMES and mapping.get("Ebs")
    ]
    if len(attached) != 1:
        names = [str(mapping.get("DeviceName")) for mapping in instance.get("BlockDeviceMappings", [])]
        raise AwsCiError(
            f"instance {instance.get('InstanceId')} must have exactly one {DATA_VOLUME_DEVICE} data volume; "
            f"found {attached} among {names}"
        )
    return attached[0]


def data_volume_id(region: str, instance_id: str) -> str:
    return data_volume_id_from_instance(describe_instance(region, instance_id))


def wait_for_device_probe(pinned: dict[str, str], volume_id: str, timeout_seconds: int = 120) -> str:
    """Wait for the guest to publish the by-id identity of the attached volume."""
    deadline = time.monotonic() + timeout_seconds
    last_error = ""
    while True:
        completed = run(ssh_command(**pinned, remote_command=device_probe_command(volume_id)), check=False, timeout=60)
        if completed.returncode == 0:
            return completed.stdout
        last_error = completed.stderr.strip() or completed.stdout.strip()
        if time.monotonic() > deadline:
            raise AwsCiError(f"volume {volume_id} never appeared by id in the guest: {last_error}")
        time.sleep(10)


def wait_for_termination(region: str, instance_id: str, timeout_seconds: int = TERMINATION_TIMEOUT_SECONDS) -> dict[str, Any]:
    deadline = time.monotonic() + timeout_seconds
    while True:
        instance = describe_instance(region, instance_id)
        if instance.get("State", {}).get("Name") == "terminated":
            return instance
        if time.monotonic() > deadline:
            raise AwsCiError(f"instance {instance_id} did not terminate within {timeout_seconds}s")
        time.sleep(15)


def ssh_keyscan_fingerprints(address: str) -> str:
    """Return the fingerprints of the host keys a fixture currently offers.

    Diagnostic only, and it runs after a pinned failure: the connection path itself
    stays pinned, so this tells an absent known_hosts entry apart from a real host
    swap, and records which key type the fixture is actually presenting.
    """
    completed = run(["ssh-keyscan", "-T", "10", address], check=False)
    offered = [line for line in completed.stdout.splitlines() if line and not line.startswith("#")]
    if not offered:
        return f"(no host key from ssh-keyscan: {completed.stderr.strip() or 'no output'})"
    fingerprints = []
    for line in offered:
        fields = line.split()
        if len(fields) >= 3:
            fingerprints.append(f"{fields[1]} {openssh_sha256_fingerprint(' '.join(fields[1:3]))}")
    return "\n".join(fingerprints) if fingerprints else "(host keys were not parsable)"


def wait_for_pinned_ssh(pinned: dict[str, str], timeout_seconds: int = SSH_TIMEOUT_SECONDS) -> None:
    """Wait for the fixture to answer with the exact pinned host key.

    A mismatch is never accepted: the pinned client refuses the connection, so
    retries only wait out the boot-time window before cloud-init installs the
    per-run host key.
    """
    deadline = time.monotonic() + timeout_seconds
    last_error = ""
    while True:
        completed = run(
            ssh_command(**pinned, remote_command="true"),
            check=False,
            timeout=30,
        )
        if completed.returncode == 0:
            return
        last_error = completed.stderr.strip()
        if time.monotonic() > deadline:
            raise AwsCiError(
                f"pinned SSH did not become available within {timeout_seconds}s; the runner never accepted a "
                f"different host key: {last_error}\n"
                f"--- host keys {pinned['address']} currently offers ---\n"
                f"{ssh_keyscan_fingerprints(pinned['address'])}"
            )
        time.sleep(SSH_POLL_SECONDS)


def command_denied_cases(args: argparse.Namespace) -> int:
    region = require_region(args.region)
    vpc_id = default_vpc(region)
    security_group_id = ci_security_group(region)
    azs = offered_azs(region, cpu_instance_type())
    if not azs:
        raise AwsCiError(f"no available {region} availability zone offers {cpu_instance_type()}")
    subnets = subnets_by_az(region, vpc_id, azs)
    if not subnets:
        raise AwsCiError("no usable default-VPC subnet in an offered availability zone")
    subnet_id = subnets[sorted(subnets)[0]]
    cases = denied_launch_cases(
        image_id=args.ami_id,
        public_ami_id=args.public_ami_id,
        instance_profile=args.instance_profile,
    )
    results: list[dict[str, str]] = []
    failed = False
    for case in cases:
        command = denied_case_command(
            case,
            region=region,
            run_id=args.run_id,
            subnet_id=subnet_id,
            security_group_id=security_group_id,
        )
        completed = aws(command[1:], check=False)
        denied = completed.returncode != 0 and "UnauthorizedOperation" in completed.stderr
        action = access_denied_action(completed.stderr) or "-"
        first_error = completed.stderr.strip().splitlines()[0] if completed.stderr.strip() else "launch succeeded"
        results.append(
            {
                "case": case["name"],
                "detail": case["detail"],
                "outcome": "denied" if denied else "NOT DENIED",
                "action": action,
                "class": error_class(completed.stderr) if completed.returncode != 0 else "none",
                "error": "" if denied else first_error,
            }
        )
        if not denied:
            failed = True
            instance_id = completed.stdout.strip().strip('"')
            if completed.returncode == 0 and instance_id:
                aws(["ec2", "terminate-instances", "--region", region, "--instance-ids", instance_id], check=False)
    lines = [
        "### Denied launch cases",
        "",
        "| case | outcome | error class | denied action |",
        "| --- | --- | --- | --- |",
    ]
    lines += [f"| {row['case']} | {row['outcome']} | `{row['class']}` | `{row['action']}` |" for row in results]
    for row in results:
        lines.append(f"- `{row['case']}`: {row['detail']}" + (f" — {row['error']}" if row["error"] else ""))
    write_summary("\n".join(lines) + "\n")
    write_outputs({"denied_cases": str(len(results)), "denied_cases_failed": str(int(failed))})
    if failed:
        raise AwsCiError("one or more denied launch cases did not fail closed with access-denied")
    return 0


def command_cleanup_run(args: argparse.Namespace) -> int:
    """Remove everything one run created, in every allowed region."""
    primary = require_region(args.region)
    ami_ids = parse_ami_ids(args.ami_ids) if args.ami_ids else {}
    removed: list[str] = []
    for region in region_order(primary):
        reservations = as_list(
            aws_json(
                [
                    "ec2",
                    "describe-instances",
                    "--region",
                    region,
                    "--filters",
                    f"Name=tag:{RUN_TAG},Values={args.run_id}",
                    "Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down",
                ]
            )["Reservations"],
            what="reservations",
        )
        leftover_instances = [
            str(instance["InstanceId"]) for reservation in reservations for instance in reservation["Instances"]
        ]
        if leftover_instances:
            aws(["ec2", "terminate-instances", "--region", region, "--instance-ids", *leftover_instances])
            removed.extend(f"instance `{instance_id}` ({region})" for instance_id in leftover_instances)
        volumes = as_list(
            aws_json(
                [
                    "ec2",
                    "describe-volumes",
                    "--region",
                    region,
                    "--filters",
                    f"Name=tag:{RUN_TAG},Values={args.run_id}",
                    "Name=status,Values=available",
                ]
            )["Volumes"],
            what="volumes",
        )
        for volume in volumes:
            aws(["ec2", "delete-volume", "--region", region, "--volume-id", str(volume["VolumeId"])])
            removed.append(f"volume `{volume['VolumeId']}` ({region})")
        snapshots = as_list(
            aws_json(
                [
                    "ec2",
                    "describe-snapshots",
                    "--region",
                    region,
                    "--owner-ids",
                    "self",
                    "--filters",
                    f"Name=tag:{RUN_TAG},Values={args.run_id}",
                ]
            )["Snapshots"],
            what="snapshots",
        )
        ami_id = ami_ids.get(region)
        if ami_id:
            aws(["ec2", "deregister-image", "--region", region, "--image-id", ami_id])
            removed.append(f"AMI `{ami_id}` ({region})")
        for snapshot in snapshots:
            aws(["ec2", "delete-snapshot", "--region", region, "--snapshot-id", str(snapshot["SnapshotId"])])
            removed.append(f"snapshot `{snapshot['SnapshotId']}` ({region})")
    for resource in removed:
        print(f"removed {resource}")
    write_summary(
        "### Smoke cleanup\n\n"
        + ("\n".join(f"- removed {resource}" for resource in removed) if removed else "- nothing to remove")
        + "\n"
    )
    return 0


def command_reap(args: argparse.Namespace) -> int:
    """Terminate aged tag-marked CI instances in every allowed region."""
    primary = require_region(args.region)
    if args.max_age_minutes < 0:
        raise AwsCiError("max age minutes must not be negative")
    now = dt.datetime.now(dt.timezone.utc)
    terminated: list[str] = []
    pending: list[dict[str, str]] = []
    foreign: list[dict[str, str]] = []
    errors: list[str] = []
    control_evidence = "-"
    for region in region_order(primary):
        reservations = as_list(
            aws_json(
                [
                    "ec2",
                    "describe-instances",
                    "--region",
                    region,
                    "--filters",
                    "Name=instance-state-name,Values=pending,running,stopping,stopped",
                ]
            )["Reservations"],
            what="reservations",
        )
        instances = [instance for reservation in reservations for instance in reservation["Instances"]]
        plan = reap_plan(instances, now=now, max_age_minutes=args.max_age_minutes)
        pending.extend(plan["pending"])
        foreign.extend(plan["foreign"])
        errors.extend(f"{region}: {error}" for error in plan["errors"])
        if args.control_instance_id and not args.dry_run:
            control = [instance for instance in instances if instance.get("InstanceId") == args.control_instance_id]
            if control:
                if instance_marker(control[0].get("Tags")) == MARKER_VALUE:
                    raise AwsCiError(f"control instance {args.control_instance_id} is tag-marked, not foreign")
                if any(entry["instance_id"] == args.control_instance_id for entry in plan["terminate"]):
                    raise AwsCiError(f"reaper selected the untagged control instance {args.control_instance_id}")
                attempt = aws(
                    ["ec2", "terminate-instances", "--region", region, "--instance-ids", args.control_instance_id],
                    check=False,
                )
                denied = attempt.returncode != 0 and "UnauthorizedOperation" in attempt.stderr
                if not denied:
                    raise AwsCiError(
                        "the CI role was able to terminate the untagged control instance; expected access-denied, "
                        f"got: {attempt.stderr.strip() or attempt.stdout.strip()}"
                    )
                control_evidence = f"denied `{access_denied_action(attempt.stderr) or 'ec2:TerminateInstances'}`"
        for decision in plan["terminate"]:
            if args.dry_run:
                terminated.append(f"{decision['instance_id']} ({region}, dry run)")
                continue
            aws(["ec2", "terminate-instances", "--region", region, "--instance-ids", decision["instance_id"]])
            terminated.append(f"{decision['instance_id']} ({region})")
    if args.control_instance_id and not args.dry_run and control_evidence == "-":
        raise AwsCiError(f"control instance {args.control_instance_id} was not found in any CI region")

    lines = [
        "### CI host reaper",
        "",
        f"- regions: {', '.join(f'`{region}`' for region in region_order(primary))}",
        f"- max age: {args.max_age_minutes} minutes ({DEADLINE_TAG} takes precedence)",
        f"- dry run: `{str(args.dry_run).lower()}`",
        f"- terminated: {', '.join(f'`{item}`' for item in terminated) if terminated else 'none'}",
        f"- not yet due: {len(pending)}",
        f"- foreign (untagged) left alone: {len(foreign)}",
        f"- untagged control denial: {control_evidence}",
    ]
    if pending:
        lines += ["", "| not yet due | deadline |", "| --- | --- |"]
        lines += [f"| `{item['instance_id']}` | {item['deadline']} |" for item in pending]
    if foreign:
        lines += ["", "| foreign instance | state | name |", "| --- | --- | --- |"]
        lines += [f"| `{item['instance_id']}` | {item['state']} | {item['name']} |" for item in foreign]
    write_summary("\n".join(lines) + "\n")
    write_outputs({"terminated": str(len(terminated)), "foreign": str(len(foreign))})
    if errors:
        raise AwsCiError("reaper could not evaluate: " + "; ".join(errors))
    return 0


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument(
        "--region",
        default=os.environ.get("AWS_CI_REGION"),
        help=f"CI primary region, one of {CI_REGIONS} (default {CI_REGION})",
    )

    render = subparsers.add_parser("render-policy", help="render the least-privilege CI role policy", parents=[common])
    render.add_argument("--account-id", required=True)
    render.add_argument("--output")
    render.set_defaults(handler=command_render_policy)

    trust = subparsers.add_parser(
        "render-trust-policy", help="render the GitHub OIDC role trust policy", parents=[common]
    )
    trust.add_argument("--account-id", default="")
    trust.add_argument("--repository", default="nunocgoncalves/iterabase-mono")
    trust.add_argument("--owner-id", default=GITHUB_OWNER_ID)
    trust.add_argument("--repository-id", default=GITHUB_REPOSITORY_ID)
    trust.add_argument("--output")
    trust.set_defaults(handler=command_render_trust_policy)

    subparsers.add_parser(
        "verify-identity", help="prove the assumed OIDC role", parents=[common]
    ).set_defaults(handler=command_verify_identity)

    bootstrap = subparsers.add_parser("bootstrap-ami", help="copy the tagged CI-owned bootstrap AMI", parents=[common])
    bootstrap.add_argument("--run-id", required=True)
    bootstrap.add_argument("--source-ami-id", default="")
    bootstrap.set_defaults(handler=command_bootstrap_ami)

    host = subparsers.add_parser("run-host", help="launch, prove, and terminate one smoke fixture", parents=[common])
    host.add_argument("--run-id", required=True)
    host.add_argument("--capacity", required=True, choices=sorted(APPROVED_INSTANCE_TYPES))
    host.add_argument("--ami-ids", required=True, help="region=ami-... map covering every CI region")
    host.set_defaults(handler=command_run_host)

    denied = subparsers.add_parser("denied-cases", help="prove the denied launch cases fail closed", parents=[common])
    denied.add_argument("--run-id", required=True)
    denied.add_argument("--ami-id", required=True)
    denied.add_argument("--public-ami-id", required=True)
    denied.add_argument("--instance-profile", required=True)
    denied.set_defaults(handler=command_denied_cases)

    cleanup = subparsers.add_parser("cleanup-run", help="remove everything the smoke run created", parents=[common])
    cleanup.add_argument("--run-id", required=True)
    cleanup.add_argument("--ami-ids", default="", help="region=ami-... map from the same run")
    cleanup.set_defaults(handler=command_cleanup_run)

    reap = subparsers.add_parser("reap", help="terminate aged tag-marked CI instances", parents=[common])
    reap.add_argument("--max-age-minutes", type=int, default=DEFAULT_MAX_AGE_MINUTES)
    reap.add_argument("--dry-run", action="store_true")
    reap.add_argument("--control-instance-id", default="")
    reap.set_defaults(handler=command_reap)

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.handler(args))
    except AwsCiError as exc:
        print(f"aws-ci error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
