# The hosted environments' own Cloudflare zone (DES-HOR-601-03). Customer
# hostnames live here, never in the mail/brand zone, so the credential every
# customer cluster holds cannot touch brand or mail DNS.

# Scoped token for in-cluster external-dns and cert-manager (DNS-01): DNS edit
# and zone read on the hosted zone only. Published as a SecureString in the
# baseline contract; HOR-593 installs it into each environment.
data "cloudflare_account_api_token_permission_groups_list" "dns_write" {
  account_id = var.cloudflare_account_id
  name       = "DNS Write"
}

data "cloudflare_account_api_token_permission_groups_list" "zone_read" {
  account_id = var.cloudflare_account_id
  name       = "Zone Read"
}

resource "cloudflare_account_token" "cluster_dns" {
  account_id = var.cloudflare_account_id
  name       = "${local.name}-cluster-dns"

  policies = [{
    effect = "allow"
    permission_groups = [
      { id = data.cloudflare_account_api_token_permission_groups_list.dns_write.result[0].id },
      { id = data.cloudflare_account_api_token_permission_groups_list.zone_read.result[0].id },
    ]
    resources = jsonencode({
      "com.cloudflare.api.account.zone.${var.hosted_zone_id}" = "*"
    })
  }]
}

# Proxied records only reach an origin over verified TLS.
resource "cloudflare_zone_setting" "hosted_ssl" {
  zone_id    = var.hosted_zone_id
  setting_id = "ssl"
  value      = "strict"
}

resource "cloudflare_zone_setting" "hosted_always_https" {
  zone_id    = var.hosted_zone_id
  setting_id = "always_use_https"
  value      = "on"
}

resource "cloudflare_zone_setting" "hosted_min_tls" {
  zone_id    = var.hosted_zone_id
  setting_id = "min_tls_version"
  value      = "1.2"
}

# Zone-level authenticated origin pulls with our own certificate
# (DES-HOR-601-04). Cloudflare presents the client certificate on every origin
# connection for the hosted zone; ingress-nginx trusts only this CA, published in
# the contract, so requests that did not come through our Cloudflare account
# fail the TLS handshake. Rotation: the client certificate renews on the first
# apply inside its renewal window; the CA does not change, so clusters need no
# update.
resource "tls_private_key" "origin_pull_ca" {
  algorithm = "RSA"
  rsa_bits  = 2048
}

resource "tls_self_signed_cert" "origin_pull_ca" {
  private_key_pem       = tls_private_key.origin_pull_ca.private_key_pem
  is_ca_certificate     = true
  validity_period_hours = 87600 # 10 years
  allowed_uses          = ["cert_signing", "crl_signing"]

  subject {
    organization = "Iterabase"
    common_name  = "Iterabase hosted origin-pull CA"
  }
}

resource "tls_private_key" "origin_pull_client" {
  algorithm = "RSA"
  rsa_bits  = 2048
}

resource "tls_cert_request" "origin_pull_client" {
  private_key_pem = tls_private_key.origin_pull_client.private_key_pem

  subject {
    organization = "Iterabase"
    common_name  = "Iterabase hosted origin pull"
  }
}

resource "tls_locally_signed_cert" "origin_pull_client" {
  cert_request_pem      = tls_cert_request.origin_pull_client.cert_request_pem
  ca_private_key_pem    = tls_private_key.origin_pull_ca.private_key_pem
  ca_cert_pem           = tls_self_signed_cert.origin_pull_ca.cert_pem
  validity_period_hours = 26280 # 3 years
  early_renewal_hours   = 2160  # renew on any apply in the last 90 days
  allowed_uses          = ["client_auth", "digital_signature", "key_encipherment"]
}

resource "cloudflare_authenticated_origin_pulls_certificate" "hosted" {
  zone_id     = var.hosted_zone_id
  certificate = tls_locally_signed_cert.origin_pull_client.cert_pem
  private_key = tls_private_key.origin_pull_client.private_key_pem

  lifecycle {
    create_before_destroy = true
  }
}

resource "cloudflare_authenticated_origin_pulls_settings" "hosted" {
  zone_id = var.hosted_zone_id
  enabled = true

  depends_on = [cloudflare_authenticated_origin_pulls_certificate.hosted]
}
