provider "aws" {
  region = var.region

  default_tags {
    tags = {
      (local.hosted_tag_key) = "baseline"
      managed-by             = "terraform/infra/hosted-aws/baseline"
    }
  }
}

# Authenticates from CLOUDFLARE_API_TOKEN: the founder's token with zone DNS edit
# and account API-token edit on the hosted zone (see docs/runbooks/aws-hosted.md).
provider "cloudflare" {}
