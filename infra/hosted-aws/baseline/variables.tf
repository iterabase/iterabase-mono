variable "region" {
  description = "Hosted account region (DES-HOR-603-04)."
  type        = string
  default     = "eu-west-1"

  validation {
    condition     = var.region == "eu-west-1"
    error_message = "The hosted account runs in eu-west-1 only (DES-HOR-603-04)."
  }
}

variable "state_bucket" {
  description = "State bucket created by the bootstrap stack."
  type        = string
}

variable "admin_principal_arns" {
  description = "IAM Identity Center administrator role ARN patterns (aws:PrincipalArn, wildcards allowed). Keep the founder's admin session here: bucket and key policies deny everyone else."
  type        = list(string)

  validation {
    condition     = length(var.admin_principal_arns) > 0
    error_message = "At least one administrator principal pattern is required, or the baseline locks itself out."
  }
}

variable "forge_operator_principal_arns" {
  description = "Principal ARN patterns allowed to assume the Forge provisioner role (operators running forge init/apply/destroy)."
  type        = list(string)
}

variable "support_principal_arns" {
  description = "Principal ARN patterns allowed to assume the support role."
  type        = list(string)
}

variable "vpc_cidr" {
  description = "Shared VPC CIDR. Must not overlap k3s cluster/service CIDRs (10.42.0.0/16, 10.43.0.0/16)."
  type        = string
  default     = "10.40.0.0/16"
}

variable "availability_zones" {
  description = "AZs that get one public subnet each."
  type        = list(string)
  default     = ["eu-west-1a", "eu-west-1b", "eu-west-1c"]
}

variable "allowed_instance_types" {
  description = "Instance types the Forge role may launch for customer environments."
  type        = list(string)
  default     = ["m7i-flex.large", "m7i-flex.xlarge", "m7i-flex.2xlarge"]
}

variable "ghcr_credential_secret_name" {
  description = "Secrets Manager secret holding the GHCR read credential for the pull-through cache. Created by the founder before apply (never in Terraform state)."
  type        = string
  default     = "ecr-pullthroughcache/ghcr"

  validation {
    condition     = startswith(var.ghcr_credential_secret_name, "ecr-pullthroughcache/")
    error_message = "ECR requires pull-through cache credentials under the ecr-pullthroughcache/ prefix."
  }
}

variable "monthly_budget_usd" {
  description = "Monthly gross cost budget for the hosted account."
  type        = number
  default     = 500
}

variable "alert_email" {
  description = "Email subscribed to budget and high-severity GuardDuty alerts (confirm the SNS subscription once)."
  type        = string
}

variable "cloudflare_account_id" {
  description = "Cloudflare account that owns the hosted zone."
  type        = string
}

variable "cloudflare_zone_id" {
  description = "Cloudflare zone of the mail domain (PIVOT-ARCH-003). Customer hostnames never live here (DES-HOR-601-03)."
  type        = string
}

variable "hosted_zone_id" {
  description = "Cloudflare zone dedicated to hosted customer environment hostnames (DES-HOR-601-03). The in-cluster DNS token and authenticated origin pulls are scoped to it."
  type        = string

  validation {
    condition     = var.hosted_zone_id != var.cloudflare_zone_id
    error_message = "Hosted environments need their own zone, separate from the mail zone (DES-HOR-601-03)."
  }
}

variable "hosted_domain" {
  description = "Apex of the hosted zone; customer hostnames are <customer>.<hosted_domain> (DES-HOR-601-03)."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9-]+(\\.[a-z0-9-]+)+$", var.hosted_domain))
    error_message = "hosted_domain must be a lowercase DNS name without a trailing dot."
  }
}

variable "mail_domain" {
  description = "SES sending domain (HOR-582), inside the Cloudflare zone."
  type        = string
}

variable "dmarc_report_email" {
  description = "Address receiving aggregate DMARC reports."
  type        = string
}
