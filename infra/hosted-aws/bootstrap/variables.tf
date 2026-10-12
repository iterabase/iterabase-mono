variable "region" {
  description = "Hosted account region (DES-HOR-603-04)."
  type        = string
  default     = "eu-west-1"

  validation {
    condition     = var.region == "eu-west-1"
    error_message = "The hosted account runs in eu-west-1 only (DES-HOR-603-04)."
  }
}
