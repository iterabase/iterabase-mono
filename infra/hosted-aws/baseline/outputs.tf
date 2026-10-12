output "contract_path" {
  description = "SSM path of the baseline consumer contract (DES-HOR-603-03)."
  value       = local.contract_path
}

output "forge_role_arn" {
  description = "Role the Forge AWS provider assumes (HOR-593)."
  value       = aws_iam_role.forge.arn
}

output "support_role_arn" {
  description = "Role support engineers assume for SSM sessions (DES-HOR-603-07)."
  value       = aws_iam_role.support.arn
}

output "node_permissions_boundary_arn" {
  description = "Permissions boundary every per-customer node role must carry (DES-HOR-603-08)."
  value       = aws_iam_policy.node_boundary.arn
}

output "image_registry_prefix" {
  description = "ECR pull-through prefix that mirrors ghcr.io (DES-HOR-603-05)."
  value       = local.contract["image-registry-prefix"]
}

output "alert_topic_arn" {
  description = "SNS topic for budget and GuardDuty alerts."
  value       = aws_sns_topic.alerts.arn
}
