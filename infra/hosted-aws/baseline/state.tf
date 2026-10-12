# Prefix separation on the bootstrap-created state bucket (DES-HOR-603-02): the
# administrators own everything, the Forge role reads and writes envs/ only, and
# every other principal is denied.
data "aws_iam_policy_document" "state_bucket" {
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = ["arn:${local.partition}:s3:::${var.state_bucket}", "arn:${local.partition}:s3:::${var.state_bucket}/*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  statement {
    sid       = "DenyUnlistedPrincipals"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = ["arn:${local.partition}:s3:::${var.state_bucket}", "arn:${local.partition}:s3:::${var.state_bucket}/*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "ArnNotLike"
      variable = "aws:PrincipalArn"
      values   = concat(local.admin_principals, [aws_iam_role.forge.arn])
    }
  }

  statement {
    sid           = "DenyForgeOutsideEnvironments"
    effect        = "Deny"
    actions       = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    not_resources = ["arn:${local.partition}:s3:::${var.state_bucket}/${local.env_prefix}*"]

    principals {
      type        = "AWS"
      identifiers = [aws_iam_role.forge.arn]
    }
  }
}

resource "aws_s3_bucket_policy" "state" {
  bucket = var.state_bucket
  policy = data.aws_iam_policy_document.state_bucket.json
}
