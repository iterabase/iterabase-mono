# Installation CA signing key (DES-HOR-661-02): non-exportable, and only the
# central task role may sign with it. Asymmetric keys cannot rotate in place.
resource "aws_kms_key" "ca" {
  description              = "Iterabase central installation CA"
  customer_master_key_spec = "ECC_NIST_P256"
  key_usage                = "SIGN_VERIFY"
  deletion_window_in_days  = 30
  policy                   = data.aws_iam_policy_document.ca_key.json
}

resource "aws_kms_alias" "ca" {
  name          = "alias/iterabase-central-ca"
  target_key_id = aws_kms_key.ca.key_id
}

data "aws_iam_policy_document" "ca_key" {
  statement {
    sid       = "AccountAdministration"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = [local.root_arn]
    }
  }

  statement {
    sid       = "CentralSigns"
    actions   = ["kms:Sign", "kms:GetPublicKey"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = [aws_iam_role.task.arn]
    }
  }

  # Administrators manage the key but never sign: nothing but the central task
  # role can issue an installation or serving certificate.
  statement {
    sid       = "OnlyCentralSigns"
    effect    = "Deny"
    actions   = ["kms:Sign"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    condition {
      test     = "ArnNotEquals"
      variable = "aws:PrincipalArn"
      values   = [aws_iam_role.task.arn]
    }
  }
}
