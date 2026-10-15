variable "region" {
  description = "Hosted account region (DES-HOR-603-04)."
  type        = string
  default     = "eu-west-1"

  validation {
    condition     = var.region == "eu-west-1"
    error_message = "The hosted account runs in eu-west-1 only (DES-HOR-603-04)."
  }
}

variable "central_image_digest" {
  description = "Digest of the published inference-gateway image the central service runs. Committed in release.auto.tfvars.json; rollout = PR + founder apply (DES-HOR-661-01)."
  type        = string

  validation {
    condition     = can(regex("^sha256:[0-9a-f]{64}$", var.central_image_digest))
    error_message = "central_image_digest must be an image digest (sha256:<64 hex>), never a tag."
  }
}

variable "provider_base_url" {
  description = "OpenAI SDK-style base URL of the managed-inference provider (TensorX), including its version path."
  type        = string

  validation {
    condition     = startswith(var.provider_base_url, "https://")
    error_message = "The provider base URL must be https."
  }
}

variable "public_host_label" {
  description = "Hostname label of the central gateway inside the hosted Cloudflare zone."
  type        = string
  default     = "central"
}

variable "admin_operator_role_arns" {
  description = "Extra role ARNs with admin scope on the operator API. The Forge role from the baseline contract is always admin."
  type        = list(string)
  default     = []
}

variable "preview_operator_role_arns" {
  description = "Role ARNs (the CI preview role) with preview scope: internal pr-*/staging installations only (DES-HOR-661-09)."
  type        = list(string)
  default     = []
}

variable "ops_alert_email" {
  description = "Email subscribed to central operations alarms."
  type        = string
}

variable "billing_alert_email" {
  description = "Email subscribed to the 80%/100% credit alerts topic (DES-HOR-661-06)."
  type        = string
}

variable "task_count" {
  description = "Central replicas, spread across availability zones."
  type        = number
  default     = 2

  validation {
    condition     = var.task_count >= 2
    error_message = "The central gateway runs at least 2 replicas (DES-HOR-661-01)."
  }
}
