# Image mirror (DES-HOR-603-05): ECR pulls platform images from ghcr.io on first
# use and keeps their digests. The credential secret is created by the founder
# before apply so its value never enters Terraform state.
data "aws_secretsmanager_secret" "ghcr" {
  name = var.ghcr_credential_secret_name
}

resource "aws_ecr_pull_through_cache_rule" "ghcr" {
  ecr_repository_prefix = "ghcr"
  upstream_registry_url = "ghcr.io"
  credential_arn        = data.aws_secretsmanager_secret.ghcr.arn
}

# Repositories ECR creates on first pull inherit encryption and a lifecycle that
# bounds cached storage.
resource "aws_ecr_repository_creation_template" "ghcr" {
  prefix               = aws_ecr_pull_through_cache_rule.ghcr.ecr_repository_prefix
  description          = "Pull-through cache of ghcr.io platform images"
  applied_for          = ["PULL_THROUGH_CACHE"]
  image_tag_mutability = "MUTABLE"

  encryption_configuration {
    encryption_type = "AES256"
  }

  lifecycle_policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the 30 most recently pulled images per repository"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = 30
      }
      action = { type = "expire" }
    }]
  })
}
