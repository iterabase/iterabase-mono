variable "name" {
  description = "Environment (installation) name; the value of the environment tag."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$", var.name))
    error_message = "name must be a lowercase DNS label of at most 32 characters."
  }
}

variable "region" {
  description = "Hosted account region (contract region, DES-HOR-603-04)."
  type        = string
}

variable "env_tag_key" {
  description = "Contract env-tag-key (iterabase-env)."
  type        = string
}

variable "vpc_id" {
  description = "Contract vpc-id: the shared hosted VPC."
  type        = string
}

variable "subnet_id" {
  description = "One of the contract's shared public subnets."
  type        = string
}

variable "instance_type" {
  description = "EC2 instance type; must be in the contract allowed-instance-types."
  type        = string
}

variable "root_volume_gib" {
  description = "Size of the encrypted gp3 root volume, in GiB."
  type        = number
}

variable "data_volume_gib" {
  description = "Size of the separate gp3 data volume (the iterabase-data blank disk), in GiB."
  type        = number
}

variable "node_role_path" {
  description = "Contract node-role-path (/iterabase/env/)."
  type        = string
}

variable "node_permissions_boundary_arn" {
  description = "Contract node-permissions-boundary-arn (DES-HOR-603-08)."
  type        = string
}

variable "lb_controller_policy_arn" {
  description = "Contract lb-controller-policy-arn (DES-HOR-601-02)."
  type        = string
}

variable "backup_bucket" {
  description = "Contract backup-bucket."
  type        = string
}

variable "backup_prefix" {
  description = "Contract backup-prefix (envs/); the node may use only <prefix><name>/."
  type        = string
}

variable "backup_kms_key_arn" {
  description = "Contract backup-kms-key-arn."
  type        = string
}

variable "session_log_bucket" {
  description = "Contract session-log-bucket for interactive Session Manager logs."
  type        = string
}

variable "session_log_group" {
  description = "Contract session-log-group for interactive Session Manager logs."
  type        = string
}

variable "mail_identity_arn" {
  description = "Contract mail-identity-arn: the verified SES sending identity."
  type        = string
}

variable "mail_user_boundary_arn" {
  description = "Contract mail-user-boundary-arn (DES-HOR-593-06)."
  type        = string
}
