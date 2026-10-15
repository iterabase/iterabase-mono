locals {
  account_id = data.aws_caller_identity.current.account_id
  partition  = data.aws_partition.current.partition
  name       = "iterabase-central"

  hosted_tag_key = "iterabase-hosted"
  root_arn       = "arn:${local.partition}:iam::${local.account_id}:root"
  contract_path  = "/iterabase/hosted/central/v1"

  # The provider marks every parameter value sensitive; only the Cloudflare
  # token is a secret, and it is read directly by the provider block.
  baseline = {
    for key, parameter in data.aws_ssm_parameter.baseline : key => nonsensitive(parameter.value)
    if key != "cloudflare-dns-token"
  }
  subnet_ids = split(",", local.baseline["subnet-ids"])

  public_host = "${var.public_host_label}.${data.cloudflare_zone.hosted.name}"

  # Pulled by digest through the ECR pull-through cache for ghcr.io
  # (DES-HOR-603-05, DES-HOR-661-01). The image ships both gateway binaries.
  image = "${local.baseline["image-registry-prefix"]}/iterabase/inference-gateway@${var.central_image_digest}"

  # Operator allow-list (DES-HOR-661-02, -09). Never committed: role ARNs come
  # from the baseline contract and the ignored terraform.tfvars.
  operators = concat(
    [{ role_arn = local.baseline["forge-role-arn"], scope = "admin" }],
    [for arn in var.admin_operator_role_arns : { role_arn = arn, scope = "admin" }],
    [for arn in var.preview_operator_role_arns : { role_arn = arn, scope = "preview" }],
  )

  # Committed price book, tiers and limits (DES-HOR-661-04, -05, -07). Only
  # the provider endpoint and public host are filled in at apply.
  central_config = templatefile("${path.module}/central.yaml", {
    public_host       = local.public_host
    provider_base_url = var.provider_base_url
  })

  # Sentinel committed until the first inference-gateway release that ships
  # cmd/central; the task definition refuses to plan with it.
  unreleased_digest = "sha256:${join("", [for _ in range(64) : "0"])}"

  listen_port = 8443
  health_port = 8081
}
