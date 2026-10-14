# Per-customer hosted environment (HOR-593). Forge embeds this module, runs it
# with a pinned Terraform binary as the baseline Forge role, and keeps its state
# in the baseline state bucket under envs/<name>/ (DES-HOR-603-02). Every input
# comes from the baseline contract /iterabase/hosted/baseline/v1/* or forge.yaml.
terraform {
  required_version = ">= 1.10.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.68"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.1"
    }
  }

  # Partial configuration: Forge passes bucket, key and region at init.
  backend "s3" {
    encrypt      = true
    use_lockfile = true
  }
}

provider "aws" {
  region = var.region

  # Tags every resource the provider creates at creation time, which the Forge
  # role's tag-on-create conditions require.
  default_tags {
    tags = local.tags
  }
}
