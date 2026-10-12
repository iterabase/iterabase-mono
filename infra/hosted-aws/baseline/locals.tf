data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id
  partition  = data.aws_partition.current.partition
  name       = "iterabase-hosted"

  # Tag contract shared with HOR-593: every per-customer resource carries
  # env_tag_key=<environment>; baseline resources carry hosted_tag_key=baseline.
  hosted_tag_key = "iterabase-hosted"
  env_tag_key    = "iterabase-env"

  # Per-customer node roles (DES-HOR-603-08) and per-environment prefixes.
  node_role_path     = "/iterabase/env/"
  node_role_arn_glob = "arn:${local.partition}:iam::${local.account_id}:role/iterabase/env/*"
  env_prefix         = "envs/"

  contract_path = "/iterabase/hosted/baseline/v1"
  root_arn      = "arn:${local.partition}:iam::${local.account_id}:root"

  # Principals the restricted buckets and key accept; everything else is denied.
  admin_principals = concat([local.root_arn], var.admin_principal_arns)
}
