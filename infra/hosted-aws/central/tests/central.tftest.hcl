# HOR-661 central stack regressions (DES-HOR-661-01/02/06/07/08/09).
# Mocked plan only: no backend state, credentials, or cloud API calls. Asserts
# the configured statements and arguments, not mocked provider output.
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

  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{}"
    }
  }

  mock_data "aws_ssm_parameter" {
    defaults = {
      value = "mock"
    }
  }
}

mock_provider "cloudflare" {
  mock_data "cloudflare_zone" {
    defaults = {
      name = "example.com"
    }
  }
}

override_data {
  target = data.aws_ssm_parameter.baseline["subnet-ids"]
  values = { value = "subnet-a,subnet-b,subnet-c" }
}

override_data {
  target = data.aws_ssm_parameter.baseline["image-registry-prefix"]
  values = { value = "123456789012.dkr.ecr.eu-west-1.amazonaws.com/ghcr" }
}

override_data {
  target = data.aws_ssm_parameter.baseline["forge-role-arn"]
  values = { value = "arn:aws:iam::123456789012:role/iterabase-hosted-forge" }
}

variables {
  central_image_digest       = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
  provider_base_url          = "https://provider.example.com/v1"
  preview_operator_role_arns = ["arn:aws:iam::210987654321:role/ci-preview"]
  admin_operator_role_arns   = ["arn:aws:iam::123456789012:role/example-admin"]
  ops_alert_email            = "ops@example.com"
  billing_alert_email        = "billing@example.com"
}

run "task_role_is_least_privilege" {
  command = plan

  assert {
    condition     = toset(flatten([for statement in data.aws_iam_policy_document.task.statement : statement.actions])) == toset(["kms:Sign", "kms:GetPublicKey", "sns:Publish"])
    error_message = "The central task role may only sign with the CA key and publish credit alerts."
  }

  assert {
    condition     = alltrue([for statement in data.aws_iam_policy_document.task.statement : statement.effect == null || statement.effect == "Allow"]) && length(data.aws_iam_policy_document.task.statement) == 2
    error_message = "The task role has exactly the CA and billing-alert statements."
  }
}

run "only_central_signs_with_the_ca_key" {
  command = plan

  assert {
    condition = (
      one([for statement in data.aws_iam_policy_document.ca_key.statement : statement if statement.sid == "OnlyCentralSigns"]).effect == "Deny" &&
      toset(one([for statement in data.aws_iam_policy_document.ca_key.statement : statement if statement.sid == "OnlyCentralSigns"]).actions) == toset(["kms:Sign"])
    )
    error_message = "Every principal but the central task role must be denied kms:Sign on the CA key (DES-HOR-661-02)."
  }

  assert {
    condition     = aws_kms_key.ca.customer_master_key_spec == "ECC_NIST_P256" && aws_kms_key.ca.key_usage == "SIGN_VERIFY"
    error_message = "The CA key is a non-exportable P-256 signing key."
  }
}

run "database_is_multi_az_and_private" {
  command = plan

  assert {
    condition     = aws_db_instance.central.multi_az && !aws_db_instance.central.publicly_accessible
    error_message = "Central RDS must be Multi-AZ and not publicly accessible (DES-HOR-661-08)."
  }

  assert {
    condition     = aws_db_instance.central.deletion_protection && aws_db_instance.central.storage_encrypted && aws_db_instance.central.backup_retention_period >= 7
    error_message = "Central RDS keeps deletion protection, encryption and 7-day backups."
  }

  assert {
    condition     = one([for parameter in aws_db_parameter_group.central.parameter : parameter.value if parameter.name == "rds.force_ssl"]) == "1"
    error_message = "Central RDS forces TLS."
  }
}

run "nlb_passes_tls_through" {
  command = plan

  assert {
    condition     = aws_lb_listener.central.port == 443 && aws_lb_listener.central.protocol == "TCP"
    error_message = "The NLB listener is TCP 443 passthrough so the tasks terminate installation mTLS (DES-HOR-661-01)."
  }

  assert {
    condition     = aws_lb_target_group.central.protocol == "TCP" && aws_lb_target_group.central.health_check[0].path == "/readyz"
    error_message = "Targets receive raw TCP and are health-checked on /readyz."
  }

  assert {
    condition     = aws_ecs_service.central.desired_count >= 2 && aws_ecs_service.central.deployment_circuit_breaker[0].rollback
    error_message = "Two or more replicas with circuit-breaker rollback (DES-HOR-661-01)."
  }
}

run "image_is_digest_pinned" {
  command = plan

  assert {
    condition     = local.image == "123456789012.dkr.ecr.eu-west-1.amazonaws.com/ghcr/iterabase/inference-gateway@${var.central_image_digest}"
    error_message = "Central runs the inference-gateway image by digest through the ECR pull-through cache."
  }
}

run "image_tag_is_rejected" {
  command = plan

  variables {
    central_image_digest = "latest"
  }

  expect_failures = [var.central_image_digest]
}

run "unreleased_sentinel_blocks_plan" {
  command = plan

  variables {
    central_image_digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  }

  expect_failures = [aws_ecs_task_definition.central]
}

run "preview_scope_only_for_preview_roles" {
  command = plan

  assert {
    condition     = toset([for operator in local.operators : operator.role_arn if operator.scope == "preview"]) == toset(var.preview_operator_role_arns)
    error_message = "Only the configured preview roles get preview scope (DES-HOR-661-09)."
  }

  assert {
    condition = toset([for operator in local.operators : operator.role_arn if operator.scope == "admin"]) == toset([
      "arn:aws:iam::123456789012:role/iterabase-hosted-forge",
      "arn:aws:iam::123456789012:role/example-admin",
    ])
    error_message = "The Forge role and configured admins are the only admin operators."
  }
}
