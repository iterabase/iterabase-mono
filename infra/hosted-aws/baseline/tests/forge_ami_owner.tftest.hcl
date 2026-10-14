# Regression for the live HOR-603 denial: Canonical's numeric owner account is
# 099720109477, but EC2 classifies its public AMI as ec2:Owner = "amazon".
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
  hosted_zone_id                = "22222222222222222222222222222222"
  hosted_domain                 = "hosted.example"
  alert_email                   = "ops@example.com"
  dmarc_report_email            = "dmarc@example.com"
  mail_domain                   = "email.example.com"
}

run "trusted_image_owner_accounts" {
  command = plan

  # Isolate the Forge policy's dependency graph, not SES/DNS/budget resources.
  plan_options {
    target = [data.aws_iam_policy_document.forge]
  }

  assert {
    condition = one(one([
      for statement in data.aws_iam_policy_document.forge.statement : statement
      if statement.sid == "LaunchFromTrustedImages"
    ]).condition).variable == "aws:ResourceAccount"
    error_message = "AMI owner IDs must use aws:ResourceAccount; ec2:Owner classifies Canonical images as amazon."
  }

  assert {
    condition = one(one([
      for statement in data.aws_iam_policy_document.forge.statement : statement
      if statement.sid == "LaunchFromTrustedImages"
      ]).condition).test == "StringEquals" && toset(one(one([
        for statement in data.aws_iam_policy_document.forge.statement : statement
        if statement.sid == "LaunchFromTrustedImages"
    ]).condition).values) == toset(["099720109477", "123456789012"])
    error_message = "The trusted AMI accounts must remain exactly Canonical and the current account, without wildcards or an amazon owner alias."
  }

  assert {
    condition = toset(one([
      for statement in data.aws_iam_policy_document.forge.statement : statement
      if statement.sid == "LaunchFromTrustedImages"
      ]).resources) == toset(["arn:aws:ec2:eu-west-1::image/*"]) && toset(one([
      for statement in data.aws_iam_policy_document.forge.statement : statement
      if statement.sid == "LaunchFromTrustedImages"
    ]).actions) == toset(["ec2:RunInstances"])
    error_message = "The owner-account permission must stay limited to RunInstances on regional AMI resources."
  }
}
