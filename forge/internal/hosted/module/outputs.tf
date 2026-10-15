output "instance_id" {
  description = "EC2 instance ID; Forge reaches it with SSH over SSM."
  value       = aws_instance.node.id
}

output "availability_zone" {
  description = "Availability zone of the instance and its data volume."
  value       = aws_instance.node.availability_zone
}

output "data_volume_id" {
  description = "Data volume ID; Forge derives its stable NVMe by-id path."
  value       = aws_ebs_volume.data.id
}

output "host_public_key" {
  description = "OpenSSH host public key installed by user-data (DES-HOR-593-02)."
  value       = trimspace(tls_private_key.host.public_key_openssh)
}

output "client_private_key" {
  description = "OpenSSH private key Forge logs in with over SSM."
  value       = tls_private_key.client.private_key_openssh
  sensitive   = true
}

output "node_role_name" {
  description = "Name of the environment node role."
  value       = aws_iam_role.node.name
}

output "security_group_id" {
  description = "The Terraform-managed node security group (no inbound rules)."
  value       = aws_security_group.node.id
}

output "smtp_username" {
  description = "SES SMTP username (the mail user's access key ID)."
  value       = aws_iam_access_key.mail.id
}

output "smtp_password" {
  description = "SES SMTP password derived for the environment's region."
  value       = aws_iam_access_key.mail.ses_smtp_password_v4
  sensitive   = true
}
