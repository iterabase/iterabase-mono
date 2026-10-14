# SES in the hosted region with one verified sending domain (HOR-582): Easy DKIM,
# a custom MAIL FROM subdomain for SPF alignment, and DMARC, all published in
# Cloudflare (PIVOT-ARCH-003). Leaving the SES sandbox is a support request in
# the runbook, not an API resource.
resource "aws_sesv2_email_identity" "mail" {
  email_identity = var.mail_domain
}

resource "aws_sesv2_email_identity_mail_from_attributes" "mail" {
  email_identity         = aws_sesv2_email_identity.mail.email_identity
  mail_from_domain       = "bounce.${var.mail_domain}"
  behavior_on_mx_failure = "REJECT_MESSAGE"
}

resource "cloudflare_dns_record" "dkim" {
  count = 3

  zone_id = var.cloudflare_zone_id
  name    = "${aws_sesv2_email_identity.mail.dkim_signing_attributes[0].tokens[count.index]}._domainkey.${var.mail_domain}"
  type    = "CNAME"
  content = "${aws_sesv2_email_identity.mail.dkim_signing_attributes[0].tokens[count.index]}.dkim.amazonses.com"
  ttl     = 3600
  proxied = false
}

resource "cloudflare_dns_record" "mail_from_mx" {
  zone_id  = var.cloudflare_zone_id
  name     = aws_sesv2_email_identity_mail_from_attributes.mail.mail_from_domain
  type     = "MX"
  content  = "feedback-smtp.${var.region}.amazonses.com"
  priority = 10
  ttl      = 3600
}

resource "cloudflare_dns_record" "mail_from_spf" {
  zone_id = var.cloudflare_zone_id
  name    = aws_sesv2_email_identity_mail_from_attributes.mail.mail_from_domain
  type    = "TXT"
  content = "\"v=spf1 include:amazonses.com -all\""
  ttl     = 3600
}

resource "cloudflare_dns_record" "dmarc" {
  zone_id = var.cloudflare_zone_id
  name    = "_dmarc.${var.mail_domain}"
  type    = "TXT"
  content = "\"v=DMARC1; p=quarantine; rua=mailto:${var.dmarc_report_email}; adkim=s; aspf=r\""
  ttl     = 3600
}
