# Infra instructions

Read the root [`AGENTS.md`](../AGENTS.md) first. Its context, Git, ticket, validation, and architecture-approval rules apply here.

## Scope

`infra/` owns account-level infrastructure as code that no product component runs itself (`DES-HOR-603-01`). Today that is the hosted AWS account's shared baseline:

| Path | Owns |
| --- | --- |
| `hosted-aws/bootstrap/` | The shared Terraform state bucket. Local state, applied once. |
| `hosted-aws/baseline/` | Shared VPC, Forge/support roles, node permissions boundary, backup bucket and KMS key, session logging, CloudTrail and Config, GuardDuty, budget and alerts, ECR pull-through cache, SES, Cloudflare DNS records and the scoped cluster DNS token, and the SSM consumer contract. |

Per-customer resources belong to the Forge AWS provider (HOR-593), not here. CI fixture accounts are `docs/runbooks/aws-ci.md` (HOR-591). The operator procedure is [`docs/runbooks/aws-hosted.md`](../docs/runbooks/aws-hosted.md).

```bash
make check      # fmt-check + validate + mocked plan tests + lint, no cloud credentials
make fmt-check
make validate   # terraform init -backend=false + validate per stack
make test       # native Terraform mocked plans, no backend state or cloud API calls
make lint       # tflint with .tflint.hcl
```

Baseline regression tests live in `hosted-aws/baseline/tests/*.tftest.hcl`. They mock both providers and assert the configured IAM statements, not the mocked policy-document JSON. A mocked plan does not prove live AWS authorization; after a reviewed/merged change and founder-approved apply, the runbook's launch dry run and live smoke remain required.

Tool versions are pinned in `.github/inputs/remote-content.json` (`ci_tools`: `terraform`, `tflint`) and installed in CI by checksum. Provider versions are locked in each stack's `.terraform.lock.hcl` for linux and darwin, amd64 and arm64; refresh with `terraform providers lock -platform=linux_amd64 -platform=linux_arm64 -platform=darwin_amd64 -platform=darwin_arm64`.

## Invariants

- **Founder applies, CI only checks** (`DES-HOR-603-02`). No workflow holds hosted-account credentials or runs `plan`/`apply`.
- **No account-identifying values or secrets in Git** (`DES-HOR-603-01`). Inputs come from an ignored `terraform.tfvars`; secrets enter AWS out of band (the GHCR pull-through credential) or are generated into state and published as SecureString (the Cloudflare cluster token).
- **The SSM contract is the only consumer interface** (`DES-HOR-603-03`). Forge reads `/iterabase/hosted/baseline/v1/*` and never the baseline state. Adding a key is compatible; renaming or removing one requires a `/v2` path and an approved decision.
- **Tag contract.** Per-customer resources carry `iterabase-env=<environment>` on the instance, its volumes and its security groups at creation; the Forge role cannot create or touch untagged resources. Baseline resources carry `iterabase-hosted=baseline`.
- **Node roles** live under IAM path `/iterabase/env/` and carry the `iterabase-hosted-node-boundary` permissions boundary (`DES-HOR-603-08`). Widening what a node may do is a boundary change here.
- **Session audit** (`DES-HOR-603-07`). Interactive Session Manager shells are logged to S3 and CloudWatch; SSH and port-forwarding sessions are audited by CloudTrail only, because AWS does not log their content.
- **Restricted buckets and key.** The state bucket, backup bucket and backup KMS key deny every principal outside the administrator patterns, the Forge role and (for backups) node roles. A wrong `admin_principal_arns` locks the administrators out; the account root remains the break-glass principal.

Any change to these invariants is architectural and requires explicit user approval before implementation.
