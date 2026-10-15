# V2 central inference gateway

HOR-661. Founder-approved decisions `DES-HOR-661-01` … `-09` are recorded on the ticket and in Obsidian `Areas/ho/Architecture/HOR-661 — Central inference gateway design decisions.md`. This document is the code-coupled contract between the central gateway (`inference-gateway/cmd/central`), installation gateways (`inference-gateway/cmd/gateway`), the control plane, Forge, the hosted infrastructure (`infra/hosted-aws/central`) and CI previews. Changing a contract here is an architectural change.

## Roles

| Component | Role |
| --- | --- |
| Central gateway | Holds provider keys, terminates installation mTLS, meters credits, owns the installation registry, entitlements, FX, alerts and the invoicing export. Runs on ECS Fargate, 2 tasks in 2 AZs, behind an NLB in TCP passthrough (`DES-HOR-661-01`). |
| Installation gateway | Routes `managed` models to central over mTLS with payload-free attribution; vLLM, SGLang and `external` (BYO-key) backends stay direct and are never metered as managed credits (`DES-HOR-661-03`). |
| Forge | Operator client of central's operator API: registers installations, issues and revokes certificates, converts plans and extends trials (`DES-HOR-661-02`, `-05`). |
| CI previews | Register `internal` preview installations and issue their certificates through Forge, with the CI preview role (`DES-HOR-661-09`). |

Self-hosted installations without managed inference never contact central.

## Identity and PKI (`DES-HOR-661-02`)

* **CA key:** an AWS KMS asymmetric key, `ECC_NIST_P256`, `SIGN_VERIFY`, alias `alias/iterabase-central-ca`. Only the central task role may `kms:Sign` / `kms:GetPublicKey`.
* **CA certificate:** self-signed by the KMS key, created once by `central ca init` (an operator subcommand run with the founder's admin session), subject `CN=Iterabase Central CA`, 10-year validity, `IsCA`, `KeyUsageCertSign|CRLSign`, path length 0. The PEM is public: it is stored in the central database (`ca` table) and published to SSM `/iterabase/hosted/central/v1/ca-bundle`; installations get it as a chart value, CI as the `CENTRAL_CA_BUNDLE` repository variable.
* **Installation certificate:** ECDSA P-256 key generated where it is used (never transits central). Forge sends a CSR. Central ignores everything in the CSR except the public key and issues:
  * subject `CN=<installation-id>`, `O=Iterabase installation`;
  * exactly one URI SAN `spiffe://central.iterabase.com/installations/<installation-id>`;
  * `ExtKeyUsageClientAuth`, `KeyUsageDigitalSignature`;
  * a random 128-bit serial, 365-day validity.
* **Central serving certificate:** issued by central itself from the same CA at start and renewed daily, 30-day validity, DNS SAN = the configured public host (default `central.iterabase.com`), `ExtKeyUsageServerAuth`. The private key exists only in task memory.
* **Installation ids:** `^[a-z][a-z0-9-]{1,61}[a-z0-9]$`. Previews use `pr-<number>`; staging uses `staging`.
* **Revocation:** registry state, not CRL/OCSP. Every installation request is checked against the in-memory registry (refreshed on Postgres `NOTIFY central_registry_changed` and every 30 s). A request is refused with `403 installation_revoked` when the installation is revoked, or the presented certificate serial is revoked, unknown, or expired. If the registry has not refreshed for 2 minutes, central fails closed with retryable `upstream_unavailable` (`DES-HOR-661-08`).

## Transport

* One public listener on `:8443` (NLB TCP 443 → 8443), TLS 1.3 only, HTTP/2 and HTTP/1.1, `ClientAuth: VerifyClientCertIfGiven` against the central CA.
* Installation routes require a verified client certificate; operator routes do not use certificates.
* Health on a plain-HTTP listener `:8081` inside the VPC: `/healthz` (process) and `/readyz` (database reachable, registry fresh, serving certificate valid). The NLB target group health-checks `/readyz`.
* Prometheus metrics on `:9090` (VPC only). Labels never carry content.

## Installation → central inference

`POST /v1/chat/completions`, OpenAI-compatible, `model` = the central model name (the installation's `ModelBackend.spec.managed.model`). The installation sends no bearer credential.

Attribution headers, all optional, opaque, payload-free, at most 128 characters each from `[A-Za-z0-9._:@/-]`; anything else is dropped (not rejected) and counted:

| Header | Source at the installation |
| --- | --- |
| `X-Request-ID` | the installation's request id |
| `X-Iterabase-Identity-Id` | effective identity of the turn |
| `X-Iterabase-Run-Id` | workflow run |
| `X-Iterabase-Turn-Id` | turn |
| `X-Iterabase-Workflow-Key` | workflow definition key; empty for chat runs |
| `X-Iterabase-Work-Item-Id` | work item driving the run, when any |
| `X-Iterabase-Channel` | `browser`, `api`, `chat`, `mailbox` or `unknown` |

The installation identity is always the certificate's, never a header.

Responses are the provider's, with the `model` field rewritten to the requested central model name. Streaming forces `stream_options.include_usage`. Errors use the OpenAI envelope and the `DES-HOR-597-02` codes, plus:

| Status | `code` | Retryable | Meaning |
| --- | --- | --- | --- |
| 403 | `installation_revoked` | no | Revoked, expired or unknown certificate/installation |
| 404 | `model_not_found` | no | Not in the price book |
| 429 | `upstream_rate_limited` | yes | Provider-wide or per-installation central limit (`DES-HOR-661-07`), or provider 429 |
| 503 | `upstream_unavailable` | yes | Provider outage, central database or registry unavailable, or a needed FX rate missing |

`GET /v1/models` lists the price book's models.

## Installation read (`DES-HOR-661-05`, `-06`)

`GET /v1/installation` (mTLS) returns:

```json
{
  "installation_id": "acme",
  "deployment": "hosted",
  "plan": "starter",
  "workflow_limit": 2,
  "trial_ends_at": null,
  "billing_currency": "EUR",
  "period": {
    "kind": "month",
    "start": "2026-10-01T00:00:00Z",
    "end": "2026-11-01T00:00:00Z",
    "allowance_credits": "5000",
    "used_credits": "1234.567891",
    "overage_credits": "0"
  },
  "alerts": [{"threshold": 80, "period_start": "2026-10-01T00:00:00Z", "crossed_at": "2026-10-20T08:00:00Z"}],
  "entitlements_version": 7,
  "as_of": "2026-10-21T09:00:00Z"
}
```

Decimal amounts are strings. `deployment` is `hosted`, `self_hosted_managed` or `internal`; `plan` is `trial`, `starter`, `growth`, `scale` or `none` (self-hosted managed and internal). `workflow_limit` is `null` when unlimited. `entitlements_version` increments on every registry change for the installation. Installations poll this endpoint (HOR-648 defines the interval and the durable cache); central never connects to installations.

## Operator API

All operator requests carry `X-Iterabase-Operator-Auth`: base64 (standard, padded) of the JSON `{"url": "...", "headers": {"...": ["..."]}, "body": "Action=GetCallerIdentity&Version=2011-06-15"}` — a SigV4-signed `POST` to the regional STS endpoint (`https://sts.eu-west-1.amazonaws.com/`). The signed headers must include `X-Iterabase-Central-Audience: <central host>`. Central rejects a URL that is not an `https://sts.<region>.amazonaws.com/` endpoint, a missing or mismatched audience, and a signature older than 15 minutes; it replays the request to STS, normalises an assumed-role ARN to its role ARN and matches the operator allow-list:

| Scope | May |
| --- | --- |
| `admin` | everything |
| `preview` | register, issue, show and revoke only `internal` installations whose id is `staging` or starts with `pr-` |

The allow-list is deployment configuration (`CENTRAL_OPERATORS`, JSON `[{"role_arn": "...", "scope": "admin"}]`), never committed.

| Method and path | Body | Result |
| --- | --- | --- |
| `POST /operator/v1/installations` | `{"id", "deployment", "plan", "billing_currency", "trial_days"?, "allowance_credits"?}` (`allowance_credits` is a decimal string, like every amount) | `201` registry entry; `200` if an identical entry exists (a revoked one is reactivated, its old certificates stay revoked); `409` if it differs |
| `GET /operator/v1/installations/{id}` | — | registry entry, certificates, current period |
| `POST /operator/v1/installations/{id}/certificates` | `{"csr_pem"}` | `{"certificate_pem", "ca_pem", "serial", "not_after"}` |
| `POST /operator/v1/installations/{id}/revoke` | `{"reason"}` | revokes the installation and all its certificates (idempotent) |
| `POST /operator/v1/installations/{id}/plan` | `{"plan"}` | convert or change a hosted plan |
| `POST /operator/v1/installations/{id}/trial-extension` | `{"days"}` | extends the trial and adds the pro-rated credits |
| `POST /operator/v1/usage/{record_id}/adjustment` | `{"classification": "company_funded", "reason"}` | audited reclassification of a verified service fault |
| `GET /operator/v1/exports/{YYYY-MM}` | `?format=json\|csv` | monthly payload-free invoicing export |

Every mutation writes an `audit_events` row with the caller's role ARN and session name.

Operator errors use the OpenAI envelope with these codes:

| Status | `code` | Meaning |
| --- | --- | --- |
| 401 | `operator_unauthenticated` | Missing, malformed, stale or wrong-audience proof, or STS rejected it |
| 403 | `operator_forbidden` | Caller not on the allow-list, or outside its scope |
| 404 | `installation_not_found` | Unknown installation |
| 409 | `installation_conflict` | Registration differs from the existing entry |
| 400 | `invalid_csr` | CSR unparsable, bad signature or not P-256 |
| 400 | `invalid_request` | Any other validation failure |

## Entitlements and periods (`DES-HOR-661-05`)

Tier contents are configuration (`tiers` in the central config):

| Plan | Workflow limit | Monthly credits |
| --- | --- | --- |
| `starter` | 2 | 5,000 |
| `growth` | 6 | 20,000 |
| `scale` | 15 | 60,000 |

* **Trial:** `trial_ends_at = registered_at + trial_days`. Its single usage period is `[trial start, trial end)` with `starter.monthly_credits × trial_days / 30` credits, and its workflow limit is Starter's. An extension moves `trial_ends_at` and adds `starter.monthly_credits × days / 30`.
* **Paid:** calendar months in UTC with the tier's full monthly grant, including the month of conversion.
* **Entitlement changes:** a plan change or trial extension rewrites the current period's rollup (allowance and end) in the same transaction, so the export and `GET /v1/installation` always agree.
* **`self_hosted_managed`:** calendar months; allowance = the registered `allowance_credits` (0 means usage-only, no alerts).
* **`internal`:** calendar months; no allowance, no alerts; all usage `company_funded`.

## Metering (`DES-HOR-661-04`)

* **Price book:** the committed `infra/hosted-aws/central/central.yaml` holds `models` (central name → provider base URL, provider model id, `currency`, `input_per_million`, `cached_input_per_million`, `output_per_million`, `effective_from`), `tiers`, `limits` and `public_host`. Its SHA-256 is the `price_version` on every usage record. The provider key comes from the `CENTRAL_PROVIDER_KEY` environment variable (ECS secret from Secrets Manager).
* **Supplier amount:** `(uncached_input × input + cached_input × cached_input_price + output × output) / 1e6` in the model's currency, from the provider's final `usage` (`prompt_tokens`, `prompt_tokens_details.cached_tokens`, `completion_tokens`).
* **FX:** central fetches the ECB 90-day reference feed (`https://www.ecb.europa.eu/stats/eurofxref/eurofxref-hist-90d.xml`) every hour and records every observation in `fx_observations`, so a fresh database can lock any current period's rate. Each billing period locks one rate in `fx_rates`, keyed by the period's UTC start date: the latest observation dated before that date, locked at the first need. A month's lock date is its first day. A trial's is its start date, and it holds for the whole trial, across month boundaries. The pre-dispatch check and the usage record both look up the rate by the installation's current period. EUR billing with a USD price: `amount / rate`; USD billing with a EUR price: `amount × rate`; same currency: no FX and no rate version. A needed rate that cannot be locked raises an operations alert and returns `upstream_unavailable`.
* **Credits:** `converted_amount / 0.01`, kept at 6 decimal places, no rounding to whole credits.
* **Classification:**
  * `company_funded` — `internal` installations, and adjustments;
  * `not_charged` — no provider usage reported (rejected before dispatch, provider error before any usage), recorded with 0 credits;
  * `chargeable` — everything else, including cancelled or failed requests that reported usage.
* **Record:** one `usage_records` row per request, written after the response completes (or fails), in the same transaction as the period rollup, the rate counters and the alert evaluation. Records keep the supplier amount and currency, price version, FX version and rate, billing currency, credits, classification, token counts, disposition and attribution. They never contain prompts, responses or provider error bodies.
* **Database outage:** if the record cannot be written, a non-streaming request returns `upstream_unavailable` instead of the provider response; a streaming request ends with the terminal `upstream_unavailable` event instead of `[DONE]`. The pre-dispatch limit check also requires the database. Central never serves unmetered inference (`DES-HOR-661-08`).

## Alerts (`DES-HOR-661-06`)

After each chargeable record, central compares the period's chargeable credits with the allowance. The first crossing of 80% and of 100% per installation and period inserts one `alerts` row (unique per installation, period and threshold) and publishes to the SNS billing topic (`CENTRAL_ALERT_TOPIC_ARN`) after commit. A background loop retries unpublished alerts. Installations see the alerts in `GET /v1/installation`.

## Rate limits (`DES-HOR-661-07`)

* `rate_counters(scope, window_start, requests, tokens)` with one-minute windows.
* Scopes are `provider:<model>` (ceiling from `limits.provider`) and `installation:<id>` (from `limits.installation`).
* Before dispatch, central admits the request against both scopes in one transaction. Each scope's tokens must be under its TPM and its requests under its RPM, and only then are both request counts incremented. A request refused by either scope consumes no quota in the other, so one installation above its own limit cannot exhaust the shared provider limit.
* After the response, it adds the tokens.
* Over a limit, central returns `429 upstream_rate_limited` with `Retry-After` set to the seconds left in the window.
* Windows older than 10 minutes are deleted by the background job.

Installation gateways use the same pattern in the control-plane database (`ratelimit.counters`, keyed by identity) in place of Redis. Redis is removed from the platform.

## Data model (central Postgres, schema `central`)

Migrations are embedded in the central binary and applied by `central migrate up` under a Postgres advisory lock, which the ECS task runs before `serve`.

* `ca` — the CA certificate PEM and KMS key ARN.
* `installations` — id, deployment, plan, billing_currency, trial_started_at, trial_ends_at, trial_credits, allowance_credits, status, entitlements_version, timestamps.
* `certificates` — serial, installation, fingerprint, not_before, not_after, issued_at, revoked_at.
* `audit_events`, `fx_observations`, `fx_rates`, `usage_records`, `period_usage`, `alerts`, `rate_counters`.

## Installation configuration

* **`ModelBackend`:** `spec.kind: managed` with `spec.managed.model: <central model name>`. The reconciler deploys nothing and marks the backend healthy. The catalog stores `kind = 'managed'`, the central model name as `backend_model_id` and an empty `backend_url`.
* **Inference-gateway chart:** `central.enabled`, `central.url` (e.g. `https://central.iterabase.com`), `central.tlsSecretName` (a `kubernetes.io/tls` Secret holding the installation key and certificate), `central.caBundle` (PEM). The gateway reads `CENTRAL_URL`, `CENTRAL_CLIENT_CERT_FILE`, `CENTRAL_CLIENT_KEY_FILE` and `CENTRAL_CA_FILE`. A `managed` model with central unconfigured fails closed with `upstream_unavailable`.

## Forge commands

`forge central <command> --central-url <url> --ca-bundle <file>`, authenticated with the ambient AWS credentials (Forge role, or the CI preview role):

* `register --installation <id> --deployment <d> --plan <p> --currency <EUR|USD> [--trial-days N] [--allowance N]`
* `issue --installation <id> --out <dir>` — generates the key locally and writes `tls.key`, `tls.crt` and `ca.crt` (mode 0600); `--secret-manifest` prints a `kubernetes.io/tls` Secret instead.
* `revoke --installation <id> --reason <text>`
* `plan --installation <id> --plan <p>`
* `extend-trial --installation <id> --days N`
* `show --installation <id>`
* `export --period YYYY-MM [--format csv|json]`

## Previews (`DES-HOR-661-09`)

* **Deploy:** the preview deploy registers `pr-<n>` (or `staging`) as `internal`/`none`/`EUR` (idempotent), issues a certificate, creates the TLS Secret and points the preview's model at a `managed` ModelBackend.
* **Teardown:** teardown and the reaper revoke the installation.
* **Credentials:** the CI preview role's ARN is on the allow-list with `preview` scope.
