# Regression for HOR-603: an assumed role's aws:userid includes its role ID,
# but the displayed SSM session ARN starts with only the role-session name.
# SSM supplies an immutable owner tag containing the full aws:userid instead.
# Mocked plan only: no backend state, credentials, or cloud API calls.
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

  # Dependency documents require valid JSON. Assert configured statements,
  # not this mocked policy-document output.
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
  hosted_zone_id                = "22222222222222222222222222222222"
  hosted_domain                 = "hosted.example"
  alert_email                   = "ops@example.com"
  dmarc_report_email            = "dmarc@example.com"
  mail_domain                   = "email.example.com"
}

run "system_tag_owns_forge_and_support_sessions" {
  command = plan

  plan_options {
    target = [data.aws_iam_policy_document.forge, data.aws_iam_policy_document.support]
  }

  assert {
    condition = alltrue([
      for statements in [data.aws_iam_policy_document.forge.statement, data.aws_iam_policy_document.support.statement] :
      one([for statement in statements : statement if statement.sid == "OwnSessions"]).effect == "Allow" &&
      toset(one([for statement in statements : statement if statement.sid == "OwnSessions"]).actions) == toset(["ssm:TerminateSession", "ssm:ResumeSession"]) &&
      toset(one([for statement in statements : statement if statement.sid == "OwnSessions"]).resources) == toset(["arn:aws:ssm:eu-west-1:123456789012:session/*"])
    ])
    error_message = "Both roles must allow only terminate/resume on sessions in the hosted account and region; the aws:userid ARN prefix does not match assumed-role sessions."
  }

  assert {
    condition = alltrue([
      for statements in [data.aws_iam_policy_document.forge.statement, data.aws_iam_policy_document.support.statement] :
      one(one([for statement in statements : statement if statement.sid == "OwnSessions"]).condition).variable == "ssm:resourceTag/aws:ssmmessages:session-id" &&
      one(one([for statement in statements : statement if statement.sid == "OwnSessions"]).condition).test == "StringEquals" &&
      toset(one(one([for statement in statements : statement if statement.sid == "OwnSessions"]).condition).values) == toset(["$${aws:userid}"])
    ])
    error_message = "Require the immutable AWS system owner tag to equal the full caller aws:userid exactly; missing tags, other roles and prefix-colliding callers must not match."
  }
}
