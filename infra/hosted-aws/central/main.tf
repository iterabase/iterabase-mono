provider "aws" {
  region = var.region

  default_tags {
    tags = {
      (local.hosted_tag_key) = "central"
      managed-by             = "terraform/infra/hosted-aws/central"
    }
  }
}

# The baseline's zone-scoped cluster DNS token (DNS Write + Zone Read on the
# hosted zone only), read from the consumer contract: no founder token needed.
provider "cloudflare" {
  api_token = data.aws_ssm_parameter.baseline["cloudflare-dns-token"].value
}
