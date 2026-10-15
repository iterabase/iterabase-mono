data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account_id]
    }
  }
}

# Execution role: pull the image through the ECR pull-through cache, write logs
# and inject the two secrets. Nothing the application itself can use.
resource "aws_iam_role" "execution" {
  name               = "${local.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

data "aws_iam_policy_document" "execution" {
  statement {
    sid       = "EcrToken"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid = "PullThroughCache"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchImportUpstreamImage",
      "ecr:CreateRepository",
    ]
    resources = ["arn:${local.partition}:ecr:${var.region}:${local.account_id}:repository/ghcr/*"]
  }

  statement {
    sid       = "Logs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.central.arn}:*"]
  }

  statement {
    sid       = "Secrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.provider_key.arn, aws_db_instance.central.master_user_secret[0].secret_arn]
  }
}

resource "aws_iam_role_policy" "execution" {
  name   = "central-execution"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution.json
}

# Task role (the application): sign with the CA key and publish credit alerts.
# sts:GetCallerIdentity replays for operator auth carry the caller's own
# signature and need no permission here.
resource "aws_iam_role" "task" {
  name               = "${local.name}-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

data "aws_iam_policy_document" "task" {
  statement {
    sid       = "InstallationCA"
    actions   = ["kms:Sign", "kms:GetPublicKey"]
    resources = [aws_kms_key.ca.arn]
  }

  statement {
    sid       = "BillingAlerts"
    actions   = ["sns:Publish"]
    resources = [aws_sns_topic.billing_alerts.arn]
  }
}

resource "aws_iam_role_policy" "task" {
  name   = "central-task"
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task.json
}
