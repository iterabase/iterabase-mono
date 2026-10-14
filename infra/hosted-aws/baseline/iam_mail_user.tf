# DES-HOR-593-06: each hosted environment sends its sign-in mail with its own
# SES SMTP credential. Forge creates one IAM user per environment under the
# node path; this boundary caps every such user at sending from the verified
# hosted identity, whatever policy is attached to it.
data "aws_iam_policy_document" "mail_user_boundary" {
  statement {
    sid       = "SendFromHostedIdentity"
    actions   = ["ses:SendRawEmail"]
    resources = [aws_sesv2_email_identity.mail.arn]
  }
}

resource "aws_iam_policy" "mail_user_boundary" {
  name        = "${local.name}-mail-user-boundary"
  path        = "/iterabase/"
  description = "Permissions boundary for per-environment SES SMTP users (DES-HOR-593-06)"
  policy      = data.aws_iam_policy_document.mail_user_boundary.json
}

# The Forge role's part: create, tag and delete users and their access keys
# only under the node path, and only with the mail boundary attached. Nothing
# may remove the boundary (NeverRemoveBoundary covers users too).
data "aws_iam_policy_document" "forge_mail_users" {
  statement {
    sid = "MailUsersWithinBoundary"
    actions = [
      "iam:CreateUser",
      "iam:PutUserPermissionsBoundary",
      "iam:PutUserPolicy",
      "iam:DeleteUserPolicy",
    ]
    resources = ["arn:${local.partition}:iam::${local.account_id}:user${local.node_role_path}*"]

    condition {
      test     = "StringEquals"
      variable = "iam:PermissionsBoundary"
      values   = [aws_iam_policy.mail_user_boundary.arn]
    }
  }

  statement {
    sid = "MailUserLifecycle"
    actions = [
      "iam:GetUser",
      "iam:GetUserPolicy",
      "iam:ListUserPolicies",
      "iam:ListAttachedUserPolicies",
      "iam:ListGroupsForUser",
      "iam:ListAccessKeys",
      "iam:ListSigningCertificates",
      "iam:ListSSHPublicKeys",
      "iam:ListServiceSpecificCredentials",
      "iam:ListMFADevices",
      "iam:GetLoginProfile",
      "iam:TagUser",
      "iam:UntagUser",
      "iam:DeleteUser",
      "iam:CreateAccessKey",
      "iam:DeleteAccessKey",
      "iam:UpdateAccessKey",
    ]
    resources = ["arn:${local.partition}:iam::${local.account_id}:user${local.node_role_path}*"]
  }

  statement {
    sid       = "ReadMailBoundary"
    actions   = ["iam:GetPolicy", "iam:GetPolicyVersion"]
    resources = [aws_iam_policy.mail_user_boundary.arn]
  }

  statement {
    sid       = "NeverRemoveMailBoundary"
    effect    = "Deny"
    actions   = ["iam:DeleteUserPermissionsBoundary"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "forge_mail_users" {
  name   = "forge-mail-users"
  role   = aws_iam_role.forge.id
  policy = data.aws_iam_policy_document.forge_mail_users.json
}
