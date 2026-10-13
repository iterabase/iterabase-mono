# The role the Forge AWS provider (HOR-593) assumes. It can only create and
# manage resources carrying the environment tag, read the baseline contract,
# write per-environment state, open SSH/port-forwarding sessions to tagged
# instances, and create node roles inside the DES-HOR-603-08 boundary.
data "aws_iam_policy_document" "forge_trust" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession", "sts:SetSourceIdentity"]

    principals {
      type        = "AWS"
      identifiers = [local.root_arn]
    }

    condition {
      test     = "ArnLike"
      variable = "aws:PrincipalArn"
      values   = var.forge_operator_principal_arns
    }
  }
}

resource "aws_iam_role" "forge" {
  name                 = "${local.name}-forge"
  description          = "Forge AWS provider: per-customer environment lifecycle (HOR-593)"
  assume_role_policy   = data.aws_iam_policy_document.forge_trust.json
  max_session_duration = 3600
}

locals {
  ec2_arn          = "arn:${local.partition}:ec2:${var.region}:${local.account_id}"
  instance_arn     = "${local.ec2_arn}:instance/*"
  volume_arn       = "${local.ec2_arn}:volume/*"
  sg_arn           = "${local.ec2_arn}:security-group/*"
  eni_arn          = "${local.ec2_arn}:network-interface/*"
  ssm_document_arn = "arn:${local.partition}:ssm:${var.region}::document"
}

data "aws_iam_policy_document" "forge" {
  statement {
    sid       = "ReadBaselineContract"
    actions   = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
    resources = ["arn:${local.partition}:ssm:${var.region}:${local.account_id}:parameter${local.contract_path}", "arn:${local.partition}:ssm:${var.region}:${local.account_id}:parameter${local.contract_path}/*"]
  }

  statement {
    sid       = "ListEnvironmentState"
    actions   = ["s3:ListBucket", "s3:ListBucketVersions"]
    resources = ["arn:${local.partition}:s3:::${var.state_bucket}"]

    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["${local.env_prefix}*"]
    }
  }

  # Teardown purges every version and delete marker under the environment's
  # prefix; the state bucket is versioned.
  statement {
    sid       = "EnvironmentState"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:GetObjectVersion", "s3:DeleteObjectVersion"]
    resources = ["arn:${local.partition}:s3:::${var.state_bucket}/${local.env_prefix}*"]
  }

  statement {
    sid       = "TeardownBackupPrefix"
    actions   = ["s3:ListBucket", "s3:ListBucketVersions"]
    resources = [aws_s3_bucket.backup.arn]

    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["${local.env_prefix}*"]
    }
  }

  statement {
    sid       = "TeardownBackupObjects"
    actions   = ["s3:DeleteObject", "s3:DeleteObjectVersion"]
    resources = ["${aws_s3_bucket.backup.arn}/${local.env_prefix}*"]
  }

  statement {
    sid = "ReadOnlyDiscovery"
    actions = [
      "ec2:Describe*",
      "elasticloadbalancing:Describe*",
      "tag:GetResources",
      "ssm:DescribeInstanceInformation",
      "ssm:DescribeSessions",
      "ssm:GetConnectionStatus",
    ]
    resources = ["*"]
  }

  # RunInstances is authorized per resource type: the instance and its volumes
  # must be created with the environment tag, from an allowed type, with IMDSv2.
  statement {
    sid       = "LaunchTaggedInstances"
    actions   = ["ec2:RunInstances"]
    resources = [local.instance_arn]

    condition {
      test     = "Null"
      variable = "aws:RequestTag/${local.env_tag_key}"
      values   = ["false"]
    }

    condition {
      test     = "StringEquals"
      variable = "ec2:InstanceType"
      values   = var.allowed_instance_types
    }

    condition {
      test     = "StringEquals"
      variable = "ec2:MetadataHttpTokens"
      values   = ["required"]
    }
  }

  statement {
    sid       = "LaunchTaggedVolumes"
    actions   = ["ec2:RunInstances", "ec2:CreateVolume"]
    resources = [local.volume_arn]

    condition {
      test     = "Null"
      variable = "aws:RequestTag/${local.env_tag_key}"
      values   = ["false"]
    }
  }

  statement {
    sid     = "LaunchIntoSharedNetwork"
    actions = ["ec2:RunInstances"]
    resources = concat(
      [for subnet in aws_subnet.public : subnet.arn],
      [local.sg_arn, local.eni_arn],
    )
  }

  # Canonical Ubuntu images or images this account owns. ec2:Owner classifies
  # public Canonical images as "amazon", not their numeric owner account ID.
  # aws:ResourceAccount checks the actual owner without allowing other AMIs.
  statement {
    sid       = "LaunchFromTrustedImages"
    actions   = ["ec2:RunInstances"]
    resources = ["arn:${local.partition}:ec2:${var.region}::image/*"]

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceAccount"
      values   = ["099720109477", local.account_id]
    }
  }

  statement {
    sid       = "CreateTaggedSecurityGroups"
    actions   = ["ec2:CreateSecurityGroup"]
    resources = [local.sg_arn]

    condition {
      test     = "Null"
      variable = "aws:RequestTag/${local.env_tag_key}"
      values   = ["false"]
    }
  }

  statement {
    sid       = "SecurityGroupsInSharedVpc"
    actions   = ["ec2:CreateSecurityGroup"]
    resources = [aws_vpc.shared.arn]
  }

  statement {
    sid       = "TagOnCreate"
    actions   = ["ec2:CreateTags"]
    resources = [local.instance_arn, local.volume_arn, local.sg_arn, local.eni_arn]

    condition {
      test     = "StringEquals"
      variable = "ec2:CreateAction"
      values   = ["RunInstances", "CreateVolume", "CreateSecurityGroup"]
    }
  }

  statement {
    sid = "ManageTaggedResources"
    actions = [
      "ec2:AttachVolume",
      "ec2:DetachVolume",
      "ec2:DeleteVolume",
      "ec2:ModifyVolume",
      "ec2:StartInstances",
      "ec2:StopInstances",
      "ec2:TerminateInstances",
      "ec2:ModifyInstanceAttribute",
      "ec2:ModifyInstanceMetadataOptions",
      "ec2:AssociateIamInstanceProfile",
      "ec2:DisassociateIamInstanceProfile",
      "ec2:AuthorizeSecurityGroupIngress",
      "ec2:AuthorizeSecurityGroupEgress",
      "ec2:RevokeSecurityGroupIngress",
      "ec2:RevokeSecurityGroupEgress",
      "ec2:DeleteSecurityGroup",
      "ec2:CreateTags",
      "ec2:DeleteTags",
    ]
    resources = [local.instance_arn, local.volume_arn, local.sg_arn]

    condition {
      test     = "Null"
      variable = "aws:ResourceTag/${local.env_tag_key}"
      values   = ["false"]
    }
  }

  # Node roles: DES-HOR-603-08. Creation and permission changes require the
  # boundary; nothing may remove it.
  statement {
    sid = "NodeRolesWithinBoundary"
    actions = [
      "iam:CreateRole",
      "iam:PutRolePermissionsBoundary",
      "iam:AttachRolePolicy",
      "iam:DetachRolePolicy",
      "iam:PutRolePolicy",
      "iam:DeleteRolePolicy",
    ]
    resources = ["arn:${local.partition}:iam::${local.account_id}:role${local.node_role_path}*"]

    condition {
      test     = "StringEquals"
      variable = "iam:PermissionsBoundary"
      values   = [aws_iam_policy.node_boundary.arn]
    }
  }

  statement {
    sid = "NodeRoleLifecycle"
    actions = [
      "iam:GetRole",
      "iam:GetRolePolicy",
      "iam:ListRolePolicies",
      "iam:ListAttachedRolePolicies",
      "iam:ListInstanceProfilesForRole",
      "iam:TagRole",
      "iam:UntagRole",
      "iam:UpdateRoleDescription",
      "iam:DeleteRole",
    ]
    resources = ["arn:${local.partition}:iam::${local.account_id}:role${local.node_role_path}*"]
  }

  statement {
    sid = "NodeInstanceProfiles"
    actions = [
      "iam:CreateInstanceProfile",
      "iam:DeleteInstanceProfile",
      "iam:GetInstanceProfile",
      "iam:AddRoleToInstanceProfile",
      "iam:RemoveRoleFromInstanceProfile",
      "iam:TagInstanceProfile",
      "iam:UntagInstanceProfile",
    ]
    resources = ["arn:${local.partition}:iam::${local.account_id}:instance-profile${local.node_role_path}*"]
  }

  statement {
    sid       = "PassNodeRolesToEc2"
    actions   = ["iam:PassRole"]
    resources = ["arn:${local.partition}:iam::${local.account_id}:role${local.node_role_path}*"]

    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ec2.amazonaws.com"]
    }
  }

  statement {
    sid       = "ReadNodeBoundary"
    actions   = ["iam:GetPolicy", "iam:GetPolicyVersion"]
    resources = [aws_iam_policy.node_boundary.arn]
  }

  statement {
    sid       = "NeverRemoveBoundary"
    effect    = "Deny"
    actions   = ["iam:DeleteRolePermissionsBoundary"]
    resources = ["*"]
  }

  # Forge's own transport: SSH and kubectl port forwarding over SSM. Interactive
  # shells are the support role's job (DES-HOR-603-07).
  statement {
    sid       = "SessionsToTaggedInstances"
    actions   = ["ssm:StartSession"]
    resources = [local.instance_arn]

    condition {
      test     = "Null"
      variable = "ssm:resourceTag/${local.env_tag_key}"
      values   = ["false"]
    }
  }

  statement {
    sid       = "SessionDocuments"
    actions   = ["ssm:StartSession"]
    resources = ["${local.ssm_document_arn}/AWS-StartSSHSession", "${local.ssm_document_arn}/AWS-StartPortForwardingSession"]
  }

  # An assumed role's session ARN starts with its role-session name, not the
  # full aws:userid. SSM's immutable system owner tag carries that full ID.
  statement {
    sid       = "OwnSessions"
    effect    = "Allow"
    actions   = ["ssm:TerminateSession", "ssm:ResumeSession"]
    resources = ["arn:${local.partition}:ssm:${var.region}:${local.account_id}:session/*"]

    condition {
      test     = "StringEquals"
      variable = "ssm:resourceTag/aws:ssmmessages:session-id"
      values   = ["$${aws:userid}"]
    }
  }

  # The client side of the session: StartSession creates it, this opens it.
  statement {
    sid       = "OwnSessionDataChannel"
    actions   = ["ssmmessages:OpenDataChannel"]
    resources = ["arn:${local.partition}:ssm:${var.region}:${local.account_id}:session/$${aws:userid}-*"]
  }
}

resource "aws_iam_role_policy" "forge" {
  name   = "forge-provisioner"
  role   = aws_iam_role.forge.id
  policy = data.aws_iam_policy_document.forge.json
}
