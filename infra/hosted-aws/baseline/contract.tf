# The baseline's consumer contract (DES-HOR-603-03). HOR-593 reads only this
# path. Adding a key is compatible; renaming or removing one requires /v2.
locals {
  contract = {
    "region"                        = var.region
    "account-id"                    = local.account_id
    "vpc-id"                        = aws_vpc.shared.id
    "subnet-ids"                    = join(",", [for zone in var.availability_zones : aws_subnet.public[zone].id])
    "env-tag-key"                   = local.env_tag_key
    "allowed-instance-types"        = join(",", var.allowed_instance_types)
    "state-bucket"                  = var.state_bucket
    "state-prefix"                  = local.env_prefix
    "backup-bucket"                 = aws_s3_bucket.backup.bucket
    "backup-prefix"                 = local.env_prefix
    "backup-kms-key-arn"            = aws_kms_key.backup.arn
    "forge-role-arn"                = aws_iam_role.forge.arn
    "support-role-arn"              = aws_iam_role.support.arn
    "node-role-path"                = local.node_role_path
    "node-permissions-boundary-arn" = aws_iam_policy.node_boundary.arn
    "session-log-bucket"            = aws_s3_bucket.session_logs.bucket
    "session-log-group"             = aws_cloudwatch_log_group.sessions.name
    "audit-log-group"               = aws_cloudwatch_log_group.trail.name
    "image-registry-prefix"         = "${local.account_id}.dkr.ecr.${var.region}.amazonaws.com/${aws_ecr_pull_through_cache_rule.ghcr.ecr_repository_prefix}"
    "mail-domain"                   = var.mail_domain
    "mail-identity-arn"             = aws_sesv2_email_identity.mail.arn
    "cloudflare-zone-id"            = var.hosted_zone_id
    "hosted-domain"                 = var.hosted_domain
    "lb-controller-policy-arn"      = aws_iam_policy.lb_controller.arn
    "origin-pull-ca-cert"           = tls_self_signed_cert.origin_pull_ca.cert_pem
    "mail-user-boundary-arn"        = aws_iam_policy.mail_user_boundary.arn
  }
}

resource "aws_ssm_parameter" "contract" {
  for_each = local.contract

  name  = "${local.contract_path}/${each.key}"
  type  = each.key == "subnet-ids" || each.key == "allowed-instance-types" ? "StringList" : "String"
  value = each.value
}

resource "aws_ssm_parameter" "cloudflare_dns_token" {
  name  = "${local.contract_path}/cloudflare-dns-token"
  type  = "SecureString"
  value = cloudflare_account_token.cluster_dns.value
}
