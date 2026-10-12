# DES-HOR-603-08: the maximum any per-customer node role may ever hold. HOR-593
# creates the role and attaches what it needs; this boundary caps it.
data "aws_iam_policy_document" "node_boundary" {
  # AmazonSSMManagedInstanceCore.
  statement {
    sid = "SsmAgent"
    actions = [
      "ssm:DescribeAssociation",
      "ssm:GetDeployablePatchSnapshotForInstance",
      "ssm:GetDocument",
      "ssm:DescribeDocument",
      "ssm:GetManifest",
      "ssm:GetParameter",
      "ssm:GetParameters",
      "ssm:ListAssociations",
      "ssm:ListInstanceAssociations",
      "ssm:PutInventory",
      "ssm:PutComplianceItems",
      "ssm:PutConfigurePackageResult",
      "ssm:UpdateAssociationStatus",
      "ssm:UpdateInstanceAssociationStatus",
      "ssm:UpdateInstanceInformation",
      "ssmmessages:CreateControlChannel",
      "ssmmessages:CreateDataChannel",
      "ssmmessages:OpenControlChannel",
      "ssmmessages:OpenDataChannel",
      "ec2messages:*",
    ]
    resources = ["*"]
  }

  # The agent uploads interactive session logs itself.
  statement {
    sid       = "SessionLogUpload"
    actions   = ["s3:PutObject", "s3:GetEncryptionConfiguration"]
    resources = [aws_s3_bucket.session_logs.arn, "${aws_s3_bucket.session_logs.arn}/*"]
  }

  statement {
    sid       = "SessionLogStream"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogGroups", "logs:DescribeLogStreams"]
    resources = ["*"]
  }

  # Platform images through the pull-through cache (DES-HOR-603-05).
  statement {
    sid = "ImagePull"
    actions = [
      "ecr:GetAuthorizationToken",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchImportUpstreamImage",
      "ecr:CreateRepository",
    ]
    resources = ["*"]
  }

  # HOR-604 backups: the environment's own prefix is enforced by the node role
  # policy; the boundary caps it to the backup bucket and key.
  statement {
    sid       = "Backups"
    actions   = ["s3:ListBucket", "s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = [aws_s3_bucket.backup.arn, "${aws_s3_bucket.backup.arn}/${local.env_prefix}*"]
  }

  statement {
    sid       = "BackupKey"
    actions   = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"]
    resources = [aws_kms_key.backup.arn]
  }

  # AWS Load Balancer Controller (HOR-601), instance-mode NLB.
  statement {
    sid = "LoadBalancerController"
    actions = [
      "elasticloadbalancing:*",
      "ec2:Describe*",
      "ec2:GetSecurityGroupsForVpc",
      "ec2:CreateSecurityGroup",
      "ec2:DeleteSecurityGroup",
      "ec2:AuthorizeSecurityGroupIngress",
      "ec2:RevokeSecurityGroupIngress",
      "ec2:CreateTags",
      "ec2:DeleteTags",
      "acm:ListCertificates",
      "acm:DescribeCertificate",
      "shield:GetSubscriptionState",
      "wafv2:GetWebACLForResource",
      "tag:GetResources",
    ]
    resources = ["*"]
  }

  statement {
    sid       = "LoadBalancerServiceLinkedRole"
    actions   = ["iam:CreateServiceLinkedRole"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "iam:AWSServiceName"
      values   = ["elasticloadbalancing.amazonaws.com"]
    }
  }
}

resource "aws_iam_policy" "node_boundary" {
  name        = "${local.name}-node-boundary"
  path        = "/iterabase/"
  description = "Permissions boundary for per-customer node roles (DES-HOR-603-08)"
  policy      = data.aws_iam_policy_document.node_boundary.json
}
