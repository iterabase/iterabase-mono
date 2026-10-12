# Interactive Session Manager logging (DES-HOR-603-07), retained for one year.
# AWS does not log SSH or port-forwarding session content; those sessions are
# covered by the CloudTrail audit in audit.tf.
resource "aws_s3_bucket" "session_logs" {
  bucket = "${local.name}-session-logs-${local.account_id}"
}

resource "aws_s3_bucket_ownership_controls" "session_logs" {
  bucket = aws_s3_bucket.session_logs.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "session_logs" {
  bucket                  = aws_s3_bucket.session_logs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "session_logs" {
  bucket = aws_s3_bucket.session_logs.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "session_logs" {
  bucket = aws_s3_bucket.session_logs.id

  rule {
    id     = "retain-one-year"
    status = "Enabled"

    filter {}

    expiration {
      days = 365
    }
  }
}

data "aws_iam_policy_document" "session_logs" {
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.session_logs.arn, "${aws_s3_bucket.session_logs.arn}/*"]

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

  # Nodes write logs; only administrators may read or remove them.
  statement {
    sid       = "DenyLogTamperingByOthers"
    effect    = "Deny"
    actions   = ["s3:GetObject", "s3:DeleteObject", "s3:DeleteObjectVersion", "s3:PutLifecycleConfiguration", "s3:PutBucketPolicy"]
    resources = [aws_s3_bucket.session_logs.arn, "${aws_s3_bucket.session_logs.arn}/*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "ArnNotLike"
      variable = "aws:PrincipalArn"
      values   = local.admin_principals
    }
  }
}

resource "aws_s3_bucket_policy" "session_logs" {
  bucket = aws_s3_bucket.session_logs.id
  policy = data.aws_iam_policy_document.session_logs.json

  depends_on = [aws_s3_bucket_public_access_block.session_logs]
}

resource "aws_cloudwatch_log_group" "sessions" {
  name              = "/iterabase/hosted/ssm-sessions"
  retention_in_days = 365
}

# Account-wide Session Manager preferences. AWS reads this exact document name.
resource "aws_ssm_document" "session_preferences" {
  name            = "SSM-SessionManagerRunShell"
  document_type   = "Session"
  document_format = "JSON"

  content = jsonencode({
    schemaVersion = "1.0"
    description   = "Iterabase hosted Session Manager preferences (DES-HOR-603-07)"
    sessionType   = "Standard_Stream"
    inputs = {
      s3BucketName                = aws_s3_bucket.session_logs.bucket
      s3KeyPrefix                 = "sessions/"
      s3EncryptionEnabled         = true
      cloudWatchLogGroupName      = aws_cloudwatch_log_group.sessions.name
      cloudWatchEncryptionEnabled = false
      cloudWatchStreamingEnabled  = true
      idleSessionTimeout          = "20"
      maxSessionDuration          = "120"
      runAsEnabled                = false
      kmsKeyId                    = ""
      shellProfile = {
        linux   = ""
        windows = ""
      }
    }
  })
}
