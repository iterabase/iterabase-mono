output "state_bucket" {
  description = "Pass to the baseline stack as -backend-config=\"bucket=...\" and as var.state_bucket."
  value       = aws_s3_bucket.state.bucket
}
