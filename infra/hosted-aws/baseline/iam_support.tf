# Support access (DES-HOR-603-07): the logged interactive Session Manager shell is
# the normal path; SSH over SSM is break-glass and audited by CloudTrail only.
data "aws_iam_policy_document" "support_trust" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession", "sts:SetSourceIdentity"]

    principals {
      type        = "AWS"
      identifiers = [local.root_arn]
    }

    condition {
      test     = "ArnLike"
      variable = "aws:PrincipalArn"
      values   = var.support_principal_arns
    }
  }
}

resource "aws_iam_role" "support" {
  name                 = "${local.name}-support"
  description          = "Support engineers: SSM sessions to customer environments"
  assume_role_policy   = data.aws_iam_policy_document.support_trust.json
  max_session_duration = 3600
}

data "aws_iam_policy_document" "support" {
  statement {
    sid = "Discovery"
    actions = [
      "ec2:DescribeInstances",
      "ssm:DescribeInstanceInformation",
      "ssm:DescribeSessions",
      "ssm:GetConnectionStatus",
    ]
    resources = ["*"]
  }

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

  # SSM-SessionManagerRunShell carries the logging preferences in session.tf.
  # Without a document a shell session uses it; the SSH document is break-glass.
  statement {
    sid       = "SessionDocuments"
    actions   = ["ssm:StartSession"]
    resources = [aws_ssm_document.session_preferences.arn, "${local.ssm_document_arn}/AWS-StartSSHSession", "${local.ssm_document_arn}/AWS-StartPortForwardingSession"]
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

resource "aws_iam_role_policy" "support" {
  name   = "support-sessions"
  role   = aws_iam_role.support.id
  policy = data.aws_iam_policy_document.support.json
}
