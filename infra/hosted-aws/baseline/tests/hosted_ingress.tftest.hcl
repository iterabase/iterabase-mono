# HOR-601 regressions: the load balancer controller policy isolates environments
# by the node role's iterabase-env tag (DES-HOR-601-02), and customer DNS,
# the cluster token and authenticated origin pulls live in the dedicated hosted
# zone, never the mail zone (DES-HOR-601-03/04). Mocked plan only: no backend
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
  # JSON. Assert the real configured statements below, not this mocked output.
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{}"
    }
  }

  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::123456789012:policy/iterabase/example"
    }
  }

  mock_data "aws_secretsmanager_secret" {
    defaults = {
      arn = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:ecr-pullthroughcache/ghcr-AbCdEf"
    }
  }

  mock_resource "aws_vpc" {
    override_during = plan
    defaults = {
      arn = "arn:aws:ec2:eu-west-1:123456789012:vpc/vpc-0123456789abcdef0"
      id  = "vpc-0123456789abcdef0"
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

run "lb_controller_isolated_by_environment" {
  command = plan

  plan_options {
    target = [data.aws_iam_policy_document.lb_controller]
  }

  # Every write either creates a resource carrying the caller's own environment
  # tag or acts on a resource that already carries it. The only exceptions are
  # the service-linked role and naming the shared VPC in CreateSecurityGroup.
  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.lb_controller.statement :
      anytrue([
        for condition in statement.condition :
        contains(["aws:RequestTag/iterabase-env", "aws:ResourceTag/iterabase-env"], condition.variable) &&
        condition.test == "StringEquals" &&
        toset(condition.values) == toset(["$${aws:PrincipalTag/iterabase-env}"])
      ])
      if !contains(["Discover", "LoadBalancerServiceLinkedRole", "CreateSecurityGroupInSharedVpc"], statement.sid)
    ])
    error_message = "Every mutating controller statement must be bound to the node role's own iterabase-env tag."
  }

  assert {
    condition = alltrue([
      for action in one([
        for statement in data.aws_iam_policy_document.lb_controller.statement : statement
        if statement.sid == "Discover"
      ]).actions :
      length(regexall(":(Describe|Get|List)", action)) > 0
    ])
    error_message = "The unconditioned discovery statement must stay read-only."
  }

  assert {
    condition = toset(one([
      for statement in data.aws_iam_policy_document.lb_controller.statement : statement
      if statement.sid == "CreateSecurityGroupInSharedVpc"
    ]).resources) == toset([aws_vpc.shared.arn])
    error_message = "Unconditioned CreateSecurityGroup must name only the shared VPC."
  }

  # Retagging may never add, change or remove the environment tag itself.
  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.lb_controller.statement :
      anytrue([
        for condition in statement.condition :
        condition.test == "ForAllValues:StringNotEquals" && condition.variable == "aws:TagKeys" &&
        toset(condition.values) == toset(["iterabase-env"])
      ])
      if contains(["RetagOwnSecurityGroups", "RetagOwnLoadBalancers"], statement.sid)
    ])
    error_message = "Retag statements must exclude the iterabase-env key."
  }

  # ForAllValues passes when aws:TagKeys is absent, and DeleteTags without Tags
  # removes every tag, iterabase-env included.
  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.lb_controller.statement :
      anytrue([
        for condition in statement.condition :
        condition.test == "Null" && condition.variable == "aws:TagKeys" && toset(condition.values) == toset(["false"])
      ])
      if contains(["RetagOwnSecurityGroups", "RetagOwnLoadBalancers"], statement.sid)
    ])
    error_message = "Retag statements must require named tag keys, so a keyless DeleteTags cannot strip iterabase-env."
  }

  assert {
    condition = alltrue([
      for statement in data.aws_iam_policy_document.lb_controller.statement :
      alltrue([for resource in statement.resources : resource != "*"])
      if contains(["ManageOwnSecurityGroups", "RetagOwnSecurityGroups", "ManageOwnLoadBalancers", "RetagOwnLoadBalancers", "TagSecurityGroupOnCreate", "TagLoadBalancersOnCreate"], statement.sid)
    ])
    error_message = "Statements on existing resources must name resource types, not *."
  }
}

run "hosted_zone_separated_from_mail" {
  command = plan

  # The generated CA is only known after apply; pin it to check the wiring.
  override_resource {
    target          = tls_self_signed_cert.origin_pull_ca
    override_during = plan
    values = {
      cert_pem = "-----BEGIN CERTIFICATE-----\nexample\n-----END CERTIFICATE-----\n"
    }
  }

  plan_options {
    target = [
      cloudflare_authenticated_origin_pulls_settings.hosted,
      cloudflare_zone_setting.hosted_ssl,
      aws_ssm_parameter.contract,
    ]
  }

  assert {
    condition     = cloudflare_authenticated_origin_pulls_settings.hosted.zone_id == var.hosted_zone_id && cloudflare_authenticated_origin_pulls_settings.hosted.enabled
    error_message = "Authenticated origin pulls must be enabled on the hosted zone."
  }

  assert {
    condition     = cloudflare_authenticated_origin_pulls_certificate.hosted.zone_id == var.hosted_zone_id
    error_message = "The origin-pull client certificate belongs to the hosted zone."
  }

  assert {
    condition     = cloudflare_zone_setting.hosted_ssl.value == "strict"
    error_message = "Hosted origins must be reached over verified TLS (Full strict)."
  }

  assert {
    condition     = aws_ssm_parameter.contract["cloudflare-zone-id"].value == var.hosted_zone_id && aws_ssm_parameter.contract["hosted-domain"].value == var.hosted_domain
    error_message = "The contract must publish the hosted zone and domain for customer hostnames."
  }

  assert {
    condition     = aws_ssm_parameter.contract["origin-pull-ca-cert"].value == tls_self_signed_cert.origin_pull_ca.cert_pem && startswith(aws_ssm_parameter.contract["origin-pull-ca-cert"].value, "-----BEGIN CERTIFICATE-----")
    error_message = "The contract must publish the origin-pull CA certificate (public, not the key)."
  }

  assert {
    condition     = contains(keys(aws_ssm_parameter.contract), "lb-controller-policy-arn")
    error_message = "The contract must publish the controller policy ARN for HOR-593."
  }
}

run "hosted_zone_must_differ_from_mail_zone" {
  command = plan

  variables {
    hosted_zone_id = "11111111111111111111111111111111"
  }

  plan_options {
    target = [cloudflare_authenticated_origin_pulls_settings.hosted]
  }

  expect_failures = [var.hosted_zone_id]
}
