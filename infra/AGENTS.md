# Infra instructions

Read the root [`AGENTS.md`](../AGENTS.md) first. Its context, Git, ticket, validation, and architecture-approval rules apply here.

## Scope

`infra/` owns account-level infrastructure as code that no product component runs itself (`DES-HOR-603-01`). Today that is the hosted AWS account's shared baseline:

| Path | Owns |
| --- | --- |
| `hosted-aws/bootstrap/` | The shared Terraform state bucket. Local state, applied once. |
| `hosted-aws/baseline/` | Shared VPC, Forge/support roles, node permissions boundary, load balancer controller policy, backup bucket and KMS key, session logging, CloudTrail and Config, GuardDuty, budget and alerts, ECR pull-through cache, SES and its mail-zone records, the hosted Cloudflare zone's settings, authenticated origin pulls and scoped cluster DNS token, and the SSM consumer contract. |
| `hosted-aws/central/` | The central inference gateway (HOR-661): ECS Fargate service, NLB, Multi-AZ RDS, KMS installation-CA key, provider-key secret, billing and ops alert topics, alarms, Cloudflare record, the committed price book `central.yaml`, the digest pin `release.auto.tfvars.json` and the `/iterabase/hosted/central/v1/*` contract. Operator procedure: [`docs/runbooks/central-gateway.md`](../docs/runbooks/central-gateway.md). |

Per-customer resources belong to the Forge AWS provider (HOR-593), not here. CI fixture accounts are `docs/runbooks/aws-ci.md` (HOR-591). The operator procedure is [`docs/runbooks/aws-hosted.md`](../docs/runbooks/aws-hosted.md).

```bash
make check      # fmt-check + validate + source guards + mocked plans + lint, no cloud credentials
make fmt-check
make validate   # terraform init -backend=false + validate per stack
make test       # Python source guards + native mocked plans, no state or cloud API calls
make lint       # tflint with .tflint.hcl
```

Python source-contract guards live in `tests/test_*.py` and prevent either hosted
stack from reintroducing Billing tag activation or the removed activation toggle
(`DES-HOR-603-09`). They inspect committed configuration only; they do not prove
live payer activation. Baseline regression tests live in
`hosted-aws/baseline/tests/*.tftest.hcl`. They mock both providers and assert the configured IAM statements, not the mocked policy-document JSON. A mocked plan does not prove live AWS authorization; after a reviewed/merged change and founder-approved apply, the runbook's launch dry run and live smoke remain required. Session-ownership regressions cover both Forge and support: terminate/resume requires SSM's immutable `aws:ssmmessages:session-id` system tag to equal the full caller `aws:userid`, not the displayed ARN prefix or a wildcard caller match. Live evidence must also prove another caller cannot terminate/resume the session; administrator teardown is not acceptance evidence.

Tool versions are pinned in `.github/inputs/remote-content.json` (`ci_tools`: `terraform`, `tflint`) and installed in CI by checksum. Provider versions are locked in each stack's `.terraform.lock.hcl` for linux and darwin, amd64 and arm64; refresh with `terraform providers lock -platform=linux_amd64 -platform=linux_arm64 -platform=darwin_amd64 -platform=darwin_arm64`.

## Invariants

- **Founder applies, CI only checks** (`DES-HOR-603-02`). No workflow holds hosted-account credentials or runs `plan`/`apply`.
- **No account-identifying values or secrets in Git** (`DES-HOR-603-01`). Inputs come from an ignored `terraform.tfvars`; secrets enter AWS out of band (the GHCR pull-through credential) or are generated into state and published as SecureString (the Cloudflare cluster token).
- **The SSM contract is the only consumer interface** (`DES-HOR-603-03`). Forge reads `/iterabase/hosted/baseline/v1/*` and never the baseline state. Adding a key is compatible; renaming or removing one requires a `/v2` path and an approved decision.
- **Tag contract.** Per-customer resources carry `iterabase-env=<environment>` on the instance, its volumes and its security groups at creation; the Forge role cannot create or touch untagged resources. Baseline resources carry `iterabase-hosted=baseline`.
- **Billing activation is payer-owned** (`DES-HOR-603-09`). Resource tagging stays in Terraform; the founder activates `iterabase-hosted` and `iterabase-env` in the Organization management account after Billing discovers them. Neither hosted stack manages this account-wide activation. Do not widen hosted IAM, add a cross-account provider or put management-account credentials in CI. Dated management-account proof that both keys are Active remains an acceptance gate.
- **Node roles** live under IAM path `/iterabase/env/` and carry the `iterabase-hosted-node-boundary` permissions boundary (`DES-HOR-603-08`). Widening what a node may do is a boundary change here.
- **Hosted zone separation** (`DES-HOR-601-03`). Customer hostnames, the in-cluster Cloudflare token, zone settings and authenticated origin pulls use `hosted_zone_id` only; the mail/brand zone (`cloudflare_zone_id`) holds SES records and is never reachable from a customer cluster. `tests/test_hosted_zone_separation.py` guards the token scope.
- **Origin pulls** (`DES-HOR-601-04`). The hosted zone presents an Iterabase client certificate on every origin connection; only the public CA enters the contract. The client key lives in baseline state only.
- **Load balancer controller isolation** (`DES-HOR-601-02`). `iterabase-hosted-lb-controller` binds every write to the calling node role's `iterabase-env` principal tag and never lets the controller change that tag. Widening it, like widening the boundary, is a change here.
- **Session audit** (`DES-HOR-603-07`). Interactive Session Manager shells are logged to S3 and CloudWatch; SSH and port-forwarding sessions are audited by CloudTrail only, because AWS does not log their content.
- **Restricted buckets and key.** The state bucket, backup bucket and backup KMS key deny every principal outside the administrator patterns, the Forge role and (for backups) node roles. A wrong `admin_principal_arns` locks the administrators out; the account root remains the break-glass principal.

- **Central gateway** (`DES-HOR-661-01`..`09`). The central stack reads the baseline only through `/iterabase/hosted/baseline/v1/*`. The image is pinned by digest in `release.auto.tfvars.json` (the all-zero sentinel refuses to plan), and rollout is PR + founder apply. Only the central task role may `kms:Sign` with the CA key; every other principal is explicitly denied. The task role has only `kms:Sign`/`kms:GetPublicKey` on that key and `sns:Publish` on the billing topic. RDS stays Multi-AZ, private, encrypted, TLS-only and deletion-protected. The NLB is TCP passthrough, because the tasks terminate installation mTLS. Operator role ARNs come from the baseline contract and the ignored `terraform.tfvars`, never Git; only `preview_operator_role_arns` get preview scope. `central.yaml` holds no secrets; the provider key lives only in Secrets Manager.

Any change to these invariants is architectural and requires explicit user approval before implementation.
