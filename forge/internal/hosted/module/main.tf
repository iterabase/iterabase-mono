data "aws_partition" "current" {}
data "aws_caller_identity" "current" {}

locals {
  full_name  = "iterabase-env-${var.name}"
  tags       = { (var.env_tag_key) = var.name }
  partition  = data.aws_partition.current.partition
  account_id = data.aws_caller_identity.current.account_id
  backup_dir = "${var.backup_prefix}${var.name}/"
  log_group  = "arn:${local.partition}:logs:${var.region}:${local.account_id}:log-group:${var.session_log_group}"
}

data "aws_subnet" "selected" {
  id = var.subnet_id
}

# Canonical Ubuntu 24.04 LTS. The image is resolved once at creation; later
# applies ignore newer images so an environment is never replaced by an AMI
# release (patching is apt, not reprovisioning).
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "architecture"
    values = ["x86_64"]
  }
}

# DES-HOR-593-02: the instance's SSH host key and the client key Forge logs in
# with are generated on the operator machine during this run. The host key is
# installed by user-data, so trust exists before the first connection. Both live
# only in user-data and this environment's state, which only the Forge role and
# administrators can read.
resource "tls_private_key" "host" {
  algorithm = "ED25519"
}

resource "tls_private_key" "client" {
  algorithm = "ED25519"
}

# Node role (DES-HOR-603-08): under the env path, capped by the baseline
# boundary, tagged with the environment so the load balancer controller policy
# can isolate environments by principal tag (DES-HOR-601-02).
resource "aws_iam_role" "node" {
  name                 = local.full_name
  path                 = var.node_role_path
  permissions_boundary = var.node_permissions_boundary_arn
  description          = "Hosted environment ${var.name} node (HOR-593)"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ssm_core" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy_attachment" "lb_controller" {
  role       = aws_iam_role.node.name
  policy_arn = var.lb_controller_policy_arn
}

# What only this environment's node may do beyond the managed policies: upload
# interactive session logs, pull images through the ECR pull-through cache, and
# use its own backup prefix and the backup key (HOR-604).
resource "aws_iam_role_policy" "node" {
  name = "environment"
  role = aws_iam_role.node.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "SessionLogUpload"
        Effect   = "Allow"
        Action   = ["s3:PutObject"]
        Resource = "arn:${local.partition}:s3:::${var.session_log_bucket}/sessions/*"
      },
      {
        Sid      = "SessionLogBucketEncryption"
        Effect   = "Allow"
        Action   = ["s3:GetEncryptionConfiguration"]
        Resource = "arn:${local.partition}:s3:::${var.session_log_bucket}"
      },
      {
        Sid      = "SessionLogStream"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
        Resource = [local.log_group, "${local.log_group}:*"]
      },
      {
        Sid      = "SessionLogGroups"
        Effect   = "Allow"
        Action   = ["logs:DescribeLogGroups"]
        Resource = "*"
      },
      {
        Sid    = "ImagePull"
        Effect = "Allow"
        Action = [
          "ecr:GetAuthorizationToken",
          "ecr:BatchGetImage",
          "ecr:GetDownloadUrlForLayer",
          "ecr:BatchCheckLayerAvailability",
          "ecr:BatchImportUpstreamImage",
          "ecr:CreateRepository",
        ]
        Resource = "*"
      },
      {
        Sid      = "BackupList"
        Effect   = "Allow"
        Action   = ["s3:ListBucket"]
        Resource = "arn:${local.partition}:s3:::${var.backup_bucket}"
        Condition = {
          StringLike = { "s3:prefix" = ["${local.backup_dir}*"] }
        }
      },
      {
        Sid      = "BackupObjects"
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
        Resource = "arn:${local.partition}:s3:::${var.backup_bucket}/${local.backup_dir}*"
      },
      {
        Sid      = "BackupKey"
        Effect   = "Allow"
        Action   = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"]
        Resource = var.backup_kms_key_arn
      },
    ]
  })
}

resource "aws_iam_instance_profile" "node" {
  name = local.full_name
  path = var.node_role_path
  role = aws_iam_role.node.name
}

# No inbound rules at all: SSH, kubectl and support go over SSM. The load
# balancer controller manages its own security groups for the NLB (HOR-601).
resource "aws_security_group" "node" {
  name        = local.full_name
  description = "Hosted environment ${var.name}: no inbound; all egress"
  vpc_id      = var.vpc_id

  egress {
    description = "All egress (SSM, ECR, package mirrors, Cloudflare, ACME)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_instance" "node" {
  ami                         = data.aws_ami.ubuntu.id
  instance_type               = var.instance_type
  subnet_id                   = var.subnet_id
  vpc_security_group_ids      = [aws_security_group.node.id]
  iam_instance_profile        = aws_iam_instance_profile.node.name
  associate_public_ip_address = true

  # IMDSv2 only, hop limit 1: ordinary pods cannot reach node credentials; only
  # the hostNetwork load balancer controller can (DES-HOR-601-05).
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }

  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.root_volume_gib
    encrypted             = true
    delete_on_termination = true
  }

  # Tags the root volume at launch (RunInstances tag specification).
  volume_tags = local.tags

  user_data = templatefile("${path.module}/user_data.yaml.tftpl", {
    hostname         = var.name
    host_private_key = tls_private_key.host.private_key_openssh
    host_public_key  = trimspace(tls_private_key.host.public_key_openssh)
    authorized_key   = trimspace(tls_private_key.client.public_key_openssh)
  })

  tags = { Name = local.full_name }

  lifecycle {
    ignore_changes = [ami, user_data]
  }
}

# The separate blank whole disk for the iterabase-data volume group; the storage
# contract is unchanged (Forge selects it by its stable NVMe identity).
resource "aws_ebs_volume" "data" {
  availability_zone = data.aws_subnet.selected.availability_zone
  size              = var.data_volume_gib
  type              = "gp3"
  encrypted         = true
  tags              = { Name = "${local.full_name}-data" }
}

resource "aws_volume_attachment" "data" {
  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.data.id
  instance_id = aws_instance.node.id
}

# DES-HOR-593-06: the environment's own SES SMTP credential, capped by the
# baseline mail boundary. Deleted with the environment and swept by tag.
resource "aws_iam_user" "mail" {
  name                 = "${local.full_name}-mail"
  path                 = var.node_role_path
  permissions_boundary = var.mail_user_boundary_arn
}

resource "aws_iam_user_policy" "mail" {
  name = "send"
  user = aws_iam_user.mail.name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "SendFromHostedIdentity"
      Effect   = "Allow"
      Action   = ["ses:SendRawEmail"]
      Resource = var.mail_identity_arn
    }]
  })
}

resource "aws_iam_access_key" "mail" {
  user = aws_iam_user.mail.name
}
