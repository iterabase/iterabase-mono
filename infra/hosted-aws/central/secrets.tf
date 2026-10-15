# The managed-inference provider key (TensorX). Terraform creates only the
# container: the founder sets the value out of band, so it never enters state
# (runbook: docs/runbooks/central-gateway.md).
resource "aws_secretsmanager_secret" "provider_key" {
  name        = "${local.name}/provider-key"
  description = "Managed-inference provider API key for the central gateway (HOR-661)."
}
