output "url" {
  description = "Public URL of the central gateway."
  value       = "https://${local.public_host}"
}

output "contract_path" {
  description = "SSM path of the central consumer contract."
  value       = local.contract_path
}

output "ca_key_arn" {
  description = "KMS key that signs installation and serving certificates (DES-HOR-661-02)."
  value       = aws_kms_key.ca.arn
}

output "cluster_name" {
  description = "ECS cluster, for run-task operations such as central ca init."
  value       = aws_ecs_cluster.central.name
}

output "task_definition_family" {
  description = "Central task definition family."
  value       = aws_ecs_task_definition.central.family
}

output "task_security_group_id" {
  description = "Security group for one-off central tasks."
  value       = aws_security_group.task.id
}

output "subnet_ids" {
  description = "Subnets for one-off central tasks."
  value       = local.subnet_ids
}

output "billing_alert_topic_arn" {
  description = "SNS topic for Iterabase's credit alerts (DES-HOR-661-06)."
  value       = aws_sns_topic.billing_alerts.arn
}
