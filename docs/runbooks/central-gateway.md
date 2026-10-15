# Central inference gateway operations

Authority: HOR-661 and its approved decisions `DES-HOR-661-01`..`09`, recorded on
the HOR-661 ticket. The service contract is
[`docs/architecture/v2-central-inference-gateway.md`](../architecture/v2-central-inference-gateway.md).
The Terraform is [`infra/hosted-aws/central/`](../../infra/hosted-aws/central),
with its invariants in [`infra/AGENTS.md`](../../infra/AGENTS.md). The stack depends
on the shared baseline ([`aws-hosted.md`](aws-hosted.md)) and reads it only
through `/iterabase/hosted/baseline/v1/*`.

## Boundary

| Surface | Contract |
| --- | --- |
| Who applies | The founder, locally, with the hosted IAM Identity Center admin session (`DES-HOR-603-02`). CI only runs `make infra-check`. |
| State | Shared state bucket, key `central/terraform.tfstate`, S3-native locking. |
| Compute | ECS Fargate service `iterabase-central`, 2 tasks (0.5 vCPU / 1 GB, x86_64) across the baseline's public subnets, deployment circuit breaker with rollback (`DES-HOR-661-01`). |
| Image | `ghcr.io/iterabase/inference-gateway` by digest through the ECR pull-through cache. The digest is committed in `release.auto.tfvars.json`; the task runs `central migrate up && exec central serve`. |
| Ingress | Internet-facing NLB, TCP 443 passthrough to tasks on 8443 (the tasks terminate installation mTLS); `/readyz` health on 8081. Cloudflare `central.<zone>` CNAME, DNS-only, written with the baseline's zone-scoped cluster DNS token. |
| Database | RDS PostgreSQL 17 `iterabase-central`, `db.t4g.small` Multi-AZ, gp3 20 GB (autoscaling to 100 GB), encrypted, TLS forced, 7-day PITR, deletion protection, master password managed by RDS in Secrets Manager. Reachable only from the task security group (`DES-HOR-661-08`). |
| CA key | KMS `alias/iterabase-central-ca`, `ECC_NIST_P256` `SIGN_VERIFY`. Only the task role can `kms:Sign`; every other principal is explicitly denied (`DES-HOR-661-02`). |
| Provider key | Secrets Manager `iterabase-central/provider-key`, value set by the founder, never in Terraform state. |
| Operators | `CENTRAL_OPERATORS`: the baseline Forge role and `admin_operator_role_arns` (admin), `preview_operator_role_arns` (preview: internal `pr-*`/`staging` only, `DES-HOR-661-09`). Values come from the baseline contract and the ignored `terraform.tfvars`. |
| Alerts | `iterabase-central-billing-alerts` (80%/100% credit crossings, `DES-HOR-661-06`) and `iterabase-central-ops-alerts` (alarms below), each with an email subscription. |
| Contract | `/iterabase/hosted/central/v1/url`, `/ca-bundle` (written by the founder after `central ca init`), `/billing-alert-topic-arn`. |
| Price book | `infra/hosted-aws/central/central.yaml` (models and supplier prices, tiers, trial basis, limits). Its SHA-256 is the price version on every usage record (`DES-HOR-661-04`). |

## First apply (founder)

Prerequisites: the baseline is applied, and an inference-gateway release that
ships `cmd/central` is published.

1. **Pin the release.** In a PR, set `central_image_digest` in
   `infra/hosted-aws/central/release.auto.tfvars.json` to that release's
   `ghcr.io/iterabase/inference-gateway` digest. The committed all-zero digest is a sentinel, and planning fails until it is replaced. Merge the PR.
2. **Apply the stack.**

   ```bash
   cd infra/hosted-aws/central
   cp terraform.tfvars.example terraform.tfvars   # fill in; ignored by Git
   terraform init -backend-config="bucket=iterabase-hosted-tfstate-<account-id>"
   terraform plan -out central.tfplan
   terraform apply central.tfplan
   ```

   The service starts but is not ready yet: there is no provider key and no CA.
   ECS keeps replacing the tasks until both exist. That is expected.
3. **Confirm both SNS email subscriptions** (billing and ops).
4. **Set the provider key** without echoing it:

   ```bash
   read -rs KEY && aws secretsmanager put-secret-value \
     --secret-id iterabase-central/provider-key --secret-string "$KEY"; unset KEY
   ```

5. **Create the CA.** Run `central ca init` as a one-off task. It runs with the task role, which is the only principal allowed to sign:

   ```bash
   CLUSTER=$(terraform output -raw cluster_name)
   SUBNETS=$(terraform output -json subnet_ids | jq -r 'join(",")')
   SG=$(terraform output -raw task_security_group_id)
   aws ecs run-task --cluster "$CLUSTER" --launch-type FARGATE \
     --task-definition "$(terraform output -raw task_definition_family)" \
     --network-configuration "awsvpcConfiguration={subnets=[$SUBNETS],securityGroups=[$SG],assignPublicIp=ENABLED}" \
     --overrides '{"containerOverrides":[{"name":"central","command":["central migrate up && central ca init"]}]}'
   ```

   `central ca init` is idempotent: it creates the CA certificate once, stores it in the central database and prints the PEM. Read the PEM from the `/iterabase/central` log stream of that task.
6. **Publish the CA bundle** to the contract and to CI:

   ```bash
   aws ssm put-parameter --name /iterabase/hosted/central/v1/ca-bundle \
     --type String --overwrite --value file://central-ca.pem
   gh variable set CENTRAL_CA_BUNDLE < central-ca.pem
   gh variable set CENTRAL_URL --body "$(terraform output -raw url)"
   ```

7. **Allow the CI preview role.** Put its ARN in `preview_operator_role_arns`, then apply again.
8. **Smoke test** with the Forge role, from an operator machine:

   ```bash
   forge central register --installation smoke-1 --deployment internal --plan none --currency EUR \
     --central-url "$CENTRAL_URL" --ca-bundle central-ca.pem
   forge central issue --installation smoke-1 --out ./smoke-1 --central-url "$CENTRAL_URL" --ca-bundle central-ca.pem
   curl --cacert central-ca.pem --cert smoke-1/tls.crt --key smoke-1/tls.key \
     "$CENTRAL_URL/v1/installation"
   curl --cacert central-ca.pem --cert smoke-1/tls.crt --key smoke-1/tls.key \
     -H 'Content-Type: application/json' "$CENTRAL_URL/v1/chat/completions" \
     -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"Say OK"}]}'
   forge central revoke --installation smoke-1 --reason "first-apply smoke" \
     --central-url "$CENTRAL_URL" --ca-bundle central-ca.pem
   curl --cacert central-ca.pem --cert smoke-1/tls.crt --key smoke-1/tls.key \
     "$CENTRAL_URL/v1/installation"   # 403 installation_revoked
   rm -rf smoke-1
   ```

## Rollout

The rollout is GitOps through a reviewed PR, applied by the founder (`DES-HOR-661-01`):

1. Bump `central_image_digest` in `release.auto.tfvars.json` to a published release digest, or change `central.yaml`. Open a PR and merge it after review.
2. Apply: `terraform plan -out central.tfplan && terraform apply central.tfplan`.
3. ECS starts two new tasks and waits for `/readyz` on the NLB. It drains old tasks over 120 seconds; streams in flight complete or end with the retryable terminal event.
4. If the new tasks never become healthy, the circuit breaker rolls the deployment back. Check `aws ecs describe-services --cluster iterabase-central --services iterabase-central`.

Migrations run before `serve` under an advisory lock and must stay additive for one release, because the previous task definition can still be running during a rollout.

## Installation operations

All operator commands authenticate with the caller's AWS identity. That means the Forge role, or an admin role on the allow-list (`DES-HOR-661-02`). Every change is audited in central.

| Task | Command |
| --- | --- |
| Register a hosted trial | `forge central register --installation <id> --deployment hosted --plan trial --trial-days 7 --currency EUR` |
| Register self-hosted managed inference | `forge central register --installation <id> --deployment self_hosted_managed --plan none --currency <EUR\|USD> --allowance <credits>` |
| Issue or re-issue a certificate (1 year) | `forge central issue --installation <id> --out <dir>`, or `--secret-manifest` to get a `kubernetes.io/tls` Secret |
| Convert a trial or change the plan | `forge central plan --installation <id> --plan starter\|growth\|scale` |
| Extend a trial | `forge central extend-trial --installation <id> --days <n>`. The trial gains Starter × n / 30 credits. |
| Inspect | `forge central show --installation <id>` |
| Revoke | `forge central revoke --installation <id> --reason <text>`. This takes effect on the next request on every replica. |
| Monthly export | `forge central export --period YYYY-MM --format csv`. The export is payload-free and goes to invoicing. |

## Alarms

All alarms go to the `iterabase-central-ops-alerts` topic.

| Alarm | Meaning | Response |
| --- | --- | --- |
| `rds-cpu-credits` | The burstable RDS instance is running out of CPU credits. | Check request volume. If it is sustained, move to a non-burstable class (`db.m7g.large`) in a PR. |
| `rds-free-storage` | Less than 2 GiB of free storage. | Storage autoscaling goes up to 100 GB. Above that, raise `max_allocated_storage`. |
| `running-tasks` | Fewer than 2 replicas are running. | Check stopped-task reasons in ECS: image pull, secret access or migration failure. One replica keeps serving. |
| `unhealthy-targets` | A replica fails `/readyz`. | `/readyz` fails when the database is unreachable, the registry is stale or the serving certificate is invalid. Check the task logs. |
| `fx-rate-unavailable` | Central could not lock an ECB rate it needed. FX-dependent managed inference is paused: steps pause and resume (`DES-HOR-661-04`). | Check egress to `www.ecb.europa.eu` and the task logs. Same-currency installations are unaffected. |

**Both replicas down, or RDS failing over:** installations get the retryable `upstream_unavailable`. Their steps pause visibly ("Model provider unavailable") and resume on their own when central recovers (HOR-649). Central never serves unmetered calls (`DES-HOR-661-08`).

## Cost

The design estimate is about €265/month of shared AWS for the whole hosted account. The central lines are on the workbook's AWS sheet, rows 46–49:

- Fargate: 2 × 0.5 vCPU / 1 GB, plus 2 task IPv4 addresses;
- the NLB;
- RDS `db.t4g.small` Multi-AZ;
- KMS, Secrets Manager, CloudWatch with Container Insights, and SNS.

Replace these estimates with the first three months of actual bills, as required by the V2 Accounting gate.
