terraform {
  required_version = ">= 1.10.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.68"
    }
  }

  # Local state by design (DES-HOR-603-02): this stack only creates the bucket
  # every other hosted-account state lives in. Keep terraform.tfstate out of Git.
}
