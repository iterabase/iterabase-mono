# The central stack reads the baseline only through its versioned consumer
# contract (DES-HOR-603-03), never through the baseline Terraform state.
locals {
  baseline_keys = toset([
    "vpc-id",
    "subnet-ids",
    "image-registry-prefix",
    "cloudflare-zone-id",
    "cloudflare-dns-token",
    "forge-role-arn",
  ])
}

data "aws_ssm_parameter" "baseline" {
  for_each = local.baseline_keys

  name            = "/iterabase/hosted/baseline/v1/${each.key}"
  with_decryption = each.key == "cloudflare-dns-token"
}

data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

data "cloudflare_zone" "hosted" {
  zone_id = local.baseline["cloudflare-zone-id"]
}
