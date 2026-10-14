# DES-HOR-593-06: per-environment SES SMTP users. Mocked plan only: no backend
# state, credentials, or cloud API calls.
mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition = "aws"
    }
  }

  # Other policy documents in the dependency graph need syntactically valid
  # JSON. Assert the real configured statement below, not this mocked output.
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{}"
    }
  }
}

mock_provider "cloudflare" {}

variables {
  state_bucket                  = "example-hosted-state"
  admin_principal_arns          = ["arn:aws:iam::123456789012:role/example-admin"]
  forge_operator_principal_arns = ["arn:aws:iam::123456789012:role/example-admin"]
  support_principal_arns        = ["arn:aws:iam::123456789012:role/example-admin"]
  cloudflare_account_id         = "00000000000000000000000000000000"
  cloudflare_zone_id            = "11111111111111111111111111111111"
  alert_email                   = "ops@example.com"
  dmarc_report_email            = "dmarc@example.com"
  mail_domain                   = "email.example.com"
}

run "mail_users_are_bounded_to_the_env_path" {
  command = plan

  plan_options {
    target = [data.aws_iam_policy_document.forge_mail_users, data.aws_iam_policy_document.mail_user_boundary]
  }

  assert {
    condition = (
      toset(one([for s in data.aws_iam_policy_document.forge_mail_users.statement : s if s.sid == "MailUsersWithinBoundary"]).actions) == toset(["iam:CreateUser", "iam:PutUserPermissionsBoundary", "iam:PutUserPolicy", "iam:DeleteUserPolicy"]) &&
      toset(one([for s in data.aws_iam_policy_document.forge_mail_users.statement : s if s.sid == "MailUsersWithinBoundary"]).resources) == toset(["arn:aws:iam::123456789012:user/iterabase/env/*"]) &&
      one(one([for s in data.aws_iam_policy_document.forge_mail_users.statement : s if s.sid == "MailUsersWithinBoundary"]).condition).variable == "iam:PermissionsBoundary" &&
      one(one([for s in data.aws_iam_policy_document.forge_mail_users.statement : s if s.sid == "MailUsersWithinBoundary"]).condition).test == "StringEquals"
    )
    error_message = "Forge may create users and change their permissions only under /iterabase/env/ and only with the mail boundary attached."
  }

  assert {
    condition     = toset(one([for s in data.aws_iam_policy_document.forge_mail_users.statement : s if s.sid == "MailUserLifecycle"]).resources) == toset(["arn:aws:iam::123456789012:user/iterabase/env/*"])
    error_message = "Mail-user lifecycle actions may reach only users under /iterabase/env/."
  }

  assert {
    condition = (
      one([for s in data.aws_iam_policy_document.forge_mail_users.statement : s if s.sid == "NeverRemoveMailBoundary"]).effect == "Deny" &&
      toset(one([for s in data.aws_iam_policy_document.forge_mail_users.statement : s if s.sid == "NeverRemoveMailBoundary"]).actions) == toset(["iam:DeleteUserPermissionsBoundary"])
    )
    error_message = "Removing a mail user's boundary must be denied."
  }

  assert {
    condition = (
      length(data.aws_iam_policy_document.mail_user_boundary.statement) == 1 &&
      toset(data.aws_iam_policy_document.mail_user_boundary.statement[0].actions) == toset(["ses:SendRawEmail"])
    )
    error_message = "A mail user may only send raw email from the hosted identity."
  }
}
