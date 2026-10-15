# Central's consumer contract for Forge, CI and HOR-593, beside the baseline's.
# Adding a key is compatible; renaming or removing one requires /v2.
resource "aws_ssm_parameter" "url" {
  name  = "${local.contract_path}/url"
  type  = "String"
  value = "https://${local.public_host}"
}

resource "aws_ssm_parameter" "billing_alert_topic_arn" {
  name  = "${local.contract_path}/billing-alert-topic-arn"
  type  = "String"
  value = aws_sns_topic.billing_alerts.arn
}

# Written by the founder after `central ca init` (runbook); Terraform creates
# the parameter and never overwrites the published bundle.
resource "aws_ssm_parameter" "ca_bundle" {
  name  = "${local.contract_path}/ca-bundle"
  type  = "String"
  value = "pending: run central ca init (docs/runbooks/central-gateway.md)"

  lifecycle {
    ignore_changes = [value]
  }
}
