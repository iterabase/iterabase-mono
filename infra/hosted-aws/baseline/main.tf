provider "aws" {
  region = var.region

  default_tags {
    tags = {
      (local.hosted_tag_key) = "baseline"
      managed-by             = "terraform/infra/hosted-aws/baseline"
    }
  }
}

# Authenticates from CLOUDFLARE_API_TOKEN: the founder's token with DNS edit on
# the mail and hosted zones, zone-settings and SSL/certificates edit on the
# hosted zone, and account API-token edit (see docs/runbooks/aws-hosted.md).
provider "cloudflare" {}
