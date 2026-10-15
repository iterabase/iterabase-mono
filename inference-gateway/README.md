# Inference Gateway

> Canonical source: [`iterabase-mono/inference-gateway`](https://github.com/nunocgoncalves/iterabase-mono/tree/master/inference-gateway). The former standalone repository is historical and read-only.

This module builds two binaries into one image (`ghcr.io/iterabase/inference-gateway`):

- **`gateway`** — the installation's OpenAI-compatible gateway. It routes
  requests to vLLM/SGLang backends, keyed external (BYO-key) providers, or the
  central Iterabase gateway for `managed` models. It consumes the model catalog,
  workload capabilities and rate limits from the **control-plane** (directly
  from the shared Postgres, LISTEN/NOTIFY-synced) and enforces per-identity rate
  limits with Postgres counters. Per installation; pairs with the control-plane
  operator, which owns the catalog/auth/policy CRDs.
- **`central`** — the central Iterabase inference gateway (HOR-661). It holds
  the provider keys, authenticates installations by mTLS certificate, meters
  credits authoritatively, and is the hosted entitlement and billing
  authority. See [Central gateway](#central-gateway-cmdcentral) and the
  contract in [`docs/architecture/v2-central-inference-gateway.md`](../docs/architecture/v2-central-inference-gateway.md).

See the Platform Direction doc (Obsidian: *Horizonshift Platform Direction*)
for the full architecture; this monorepo directory is the source of truth for
gateway infrastructure intent.

## Status

**Walking skeleton.** HOR-247 rewired the gateway onto the control-plane
snapshot: it reads `catalog.effective_catalog`,
`permissions.effective_capabilities`, and `permissions.effective_rate_limits`
directly from the shared Postgres (in-memory cache, LISTEN/NOTIFY-refreshed)
and routes alias → backend. The legacy self-managed registry / auth / admin /
load balancer is gone. Requires control-plane v0.0.5 (HOR-328) on the shared
Postgres.

**Customer listener (HOR-582).** Legacy scoped API keys and the
`identity.active_api_keys` lookup retired with the control-plane's V2-only
authority (`DES-HOR-582-01`). Until HOR-513 delivers live V2 credential
enforcement (`identity.inference_api_credentials`), the customer listener
denies every bearer request before any backend call. The workload mTLS
listener is unaffected.

## Architecture

```
Direct callers (customer bearer)
  ──> [ Auth: denied until HOR-513 V2 credentials ]

Supervisor (workload mTLS, HOR-398)            │
  ──> [ mTLS h2 + SPIFFE ] ──> [ WorkloadAuth: pool + active turn scope ]
      ──> [ RateLimit: effective identity (Postgres) ] ──> same proxy pipeline ──┘
                                │
                  snapshot cache (catalog / capabilities / rate limits)
                                │
            shared Postgres (control-plane schemas, incl. ratelimit.counters)
```

- **Catalog / workload capability / rate-limits:** read-only from the
  control-plane's Postgres views (`catalog.effective_catalog`,
  `permissions.effective_capabilities`, `permissions.effective_rate_limits`).
  The gateway **owns no tables** (no `migrate` subcommand; the control-plane
  creates the schemas).
- **Snapshot cache:** in-memory, per pod; refreshed on start, on LISTEN/NOTIFY
  (`catalog_changed` / `permissions_changed`), and on a 30s
  poll fallback. `/readyz` goes unhealthy if the snapshot is stale, so a pod with
  a dead LISTEN drops from the Service LB. Multi-replica-safe: each pod LISTENs.
- **Routing:** alias → single `backend_url` (the catalog's `available` flag is
  the health signal, set by the control-plane `ModelBackend` reconciler). The
  request `model` field is rewritten alias → `backend_model_id`; per-alias
  transforms applied (default params, reasoning config, system prompt). No
  weighted round-robin / health checker.
- **External providers (HOR-597, DES-HOR-597-01):** a keyed `external`
  `ModelBackend` publishes only its `authRef` Secret *name* in the catalog. The
  gateway reads that Secret's `key` from the Kubernetes API in its own
  namespace (ServiceAccount with namespace-scoped `get` on Secrets), holds the
  value in memory (stale on every snapshot refresh, at most 1 minute old), and
  sends it upstream as `Authorization: Bearer`. It is never logged, never
  stored in Postgres, and never reaches workloads; the client's own credential
  is never forwarded. Outside a cluster, keyed backends fail closed. Request
  bodies, including image content parts, pass through unchanged.
  An external `baseURL` is the provider's OpenAI SDK base including its
  version path (`https://provider/v1`); requests go to
  `<baseURL>/chat/completions` (DES-HOR-597-04). In-cluster backends keep
  `<service URL>/v1/chat/completions`.
- **Managed models (HOR-661, DES-HOR-661-03):** a `managed` `ModelBackend`
  carries only the central model name. The gateway sends it to
  `CENTRAL_URL/v1/chat/completions` over mTLS with the installation's
  central-issued certificate (re-read when the Secret is renewed), never a
  bearer, and with payload-free attribution headers (identity, run, turn,
  workflow key, work item, channel). Without a central route a managed model
  fails closed with `upstream_unavailable`. vLLM/SGLang and `external`
  (BYO-key) backends never touch central and consume no managed credits.
- **Rate limits (DES-HOR-661-07):** per-identity RPM/TPM (from the snapshot)
  enforced with exact one-minute counters in the control-plane Postgres
  (`ratelimit.counters`), shared by every replica. TPM is incremented from
  response usage. Rate limiting fails open on database errors. Windows older
  than ten minutes are pruned every minute. There is no Redis.
- **Supervisor workload path (HOR-398; ARCH-010/011):** a second HTTP/2 mTLS
  listener (`workload.enabled`) accepts the trusted harness supervisor using
  its SPIFFE-bound workload identity. `WorkloadAuth` resolves the pool from the
  verified SPIFFE id and validates the active durable turn (`runtime.turns` +
  `runtime.run_pool_assignments`, read live on the request path — not cached)
  against `X-Iterabase-Run-Id` / `X-Iterabase-Turn-Id`. The run's
  `scope_identity_id` becomes the effective identity (catalogue authz, usage,
  rate limits share the proxy pipeline); the requested model must equal the
  turn's assigned model (deny model-mismatched). Stale/fenced/inactive/terminal
  turns are denied without fallback. The disposable child receives no
  credential — the mTLS cert is held by the supervisor only.

## Quick start

The gateway reads the control-plane's schemas, so run it alongside a
control-plane (whose migrations create the schemas). The model aliases come
from control-plane `Model` CRs.

```bash
export DATABASE_URL="postgres://..."   # shared Postgres (control-plane schemas)
export ADMIN_API_KEY="..."              # for /admin/v1/snapshot
go run ./cmd/gateway serve
```

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer cp-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3-27b","messages":[{"role":"user","content":"Hello!"}]}'
```

## Configuration

YAML and/or env vars (env wins). Key sections:

```yaml
server:       {port: 8080, read_timeout: 30s, write_timeout: 300s, idle_timeout: 120s}
database:     {url: "${DATABASE_URL}", max_open_conns: 25, max_idle_conns: 10}
central:      {url: "${CENTRAL_URL}", client_cert_file: ..., client_key_file: ..., ca_file: ...}
auth:         {admin_key: "${ADMIN_API_KEY}"}        # /admin/v1/snapshot only
snapshot:
  refresh_interval: 30s       # poll fallback (LISTEN/NOTIFY drives prompt updates)
  readiness_staleness: 60s    # /readyz unhealthy if older than this
logging:      {level: info, format: json}
```

| Env var | Config | |
|---|---|---|
| `DATABASE_URL` | `database.url` | shared Postgres (required) |
| `CENTRAL_URL` | `central.url` | central gateway for `managed` models (optional) |
| `CENTRAL_CLIENT_CERT_FILE` / `CENTRAL_CLIENT_KEY_FILE` / `CENTRAL_CA_FILE` | `central.*` | installation certificate, key and central CA (required with `CENTRAL_URL`) |
| `ADMIN_API_KEY` | `auth.admin_key` | debug endpoint |
| `PORT` | `server.port` | |
| `METRICS_ENABLED` / `METRICS_PORT` | `metrics.*` | Dedicated in-cluster listener |
| `LOG_LEVEL` / `LOG_FORMAT` | `logging.*` | |

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | Customer bearer (denied until HOR-513) | Proxy to the backend (streaming + non-streaming) |
| `GET` | `/v1/models` | Customer bearer (denied until HOR-513) | Available aliases |
| `GET` | `/health` | – | Liveness |
| `GET` | `/readyz` | – | Readiness (snapshot freshness gate) |
| `GET` | `/metrics` on the dedicated metrics port | – | Prometheus; never exposed through API ingress or workload mTLS |
| `GET` | `/admin/v1/snapshot` | `X-Admin-Key` | Consumed catalog / debug (read-only, no secrets) |

Provider failures are classified in the OpenAI error envelope
(DES-HOR-597-02); the runtime pauses on the retryable codes (HOR-649):

| Provider condition | Response | `error.code` | Retryable |
|---|---|---|---|
| Unreachable, timeout, or any `5xx` (including `529` and a CDN's `520`–`524`) | `503` + `Retry-After` | `upstream_unavailable` | yes |
| `429` | `429` + `Retry-After` | `upstream_rate_limited` | yes |
| `401`/`403`, or the `authRef` credential is unavailable | `502` | `upstream_auth_failed` | no |
| Other `4xx` | forwarded unchanged | provider's | no |

The status alone decides a classified failure; the provider's error body is
never read or passed through. `Retry-After` keeps the provider's value in
either form (seconds or HTTP-date), else `5`. When a stream fails after it
started — the connection drops, it ends without `[DONE]`, or the provider sends
anything but a completion chunk (an error envelope, an `error` event, non-JSON
data; all discarded) — the gateway ends it with one
`data: {"error":{"code":"upstream_unavailable",...}}` event and no `[DONE]`.
The provider stream is parsed as whole SSE events in any valid framing (with or
without the space after `data:`, multi-line data, LF/CRLF/CR line ends) and
re-emitted as single-line `data: ` events; provider comments become a bare
`: keep-alive`, and `id`/`retry` fields are dropped.
Each classified failure increments `gateway_upstream_errors_total{model,code}`.

`/v1` responses carry rate-limit headers (`X-RateLimit-Limit/Remaining/Reset-Requests`
+ `-Tokens`); `429` + `Retry-After` when exceeded. The request `model` field uses
the alias; the response `model` is rewritten back to the alias.

## Development

```bash
make build          # bin/gateway and bin/central
make test           # requires Docker (testcontainers: Postgres)
make lint           # golangci-lint
```

```
cmd/gateway/main.go        serve
cmd/central/main.go        serve | migrate up | ca init
internal/
  attribution/             payload-free installation -> central attribution headers
  centrallink/             installation mTLS client to central (certificate reload)
  central/                 the central gateway (see below)
  config/                  configuration
  database/                PG connection pool (no migrations — gateway owns no tables)
  snapshot/                control-plane views reader + in-memory LISTEN/NOTIFY cache
  proxy/                   routing + model rewrite + transforms; the shared Forwarder (round trip, SSE, classification, usage)
  middleware/              customer auth (denied until HOR-513), workload mTLS auth, rate limit, logging, metrics, request ID
  ratelimit/               Postgres one-minute counters (RPM/TPM) + stream TPM batcher
  testpg/                  testcontainers Postgres for integration tests
  server/                  HTTP server + routes (/v1, /readyz, /admin/v1/snapshot); workload mTLS listener
  spiffe/                  SPIFFE workload-identity parse + mTLS server TLS config (+ testca)
  workload/                live durable-state resolution (pool + active turn scope)
  metrics/                 Prometheus definitions
```

## Central gateway (`cmd/central`)

Runs on ECS Fargate in the hosted account (`infra/hosted-aws/central`,
DES-HOR-661-01): two tasks behind an NLB in TCP passthrough, RDS PostgreSQL
Multi-AZ. The container runs `central migrate up && exec central serve`.

| Listener | Purpose |
|---|---|
| `:8443` TLS 1.3 | `POST /v1/chat/completions`, `GET /v1/models`, `GET /v1/installation` (installation certificate required); `/operator/v1/*` (presigned STS proof) |
| `:8081` HTTP | `/healthz`, `/readyz` (database, registry freshness, serving certificate) |
| `:9090` HTTP | Prometheus |

| Env var | |
|---|---|
| `CENTRAL_CONFIG` | Price book and policy YAML (`infra/hosted-aws/central/central.yaml`, rendered); its SHA-256 is the price version |
| `CENTRAL_PUBLIC_HOST` | Serving certificate name and operator audience |
| `CENTRAL_DATABASE_HOST` / `_PORT` / `_NAME` / `_USER` / `_PASSWORD` / `_SSLMODE` | RDS connection (or `CENTRAL_DATABASE_URL`) |
| `CENTRAL_KMS_KEY_ID` | KMS ECC_NIST_P256 key that signs as the installation CA |
| `CENTRAL_PROVIDER_KEY` | Provider API key (Secrets Manager) |
| `CENTRAL_OPERATORS` | JSON allow-list `[{"role_arn", "scope": "admin"\|"preview"}]` |
| `CENTRAL_ALERT_TOPIC_ARN` | SNS billing-alerts topic (credit and FX alerts) |
| `AWS_REGION` | KMS/SNS region |

- **Commands:** `central ca init` creates the CA certificate from the KMS key
  once (idempotent) and prints its PEM, which is published as the installations'
  trust bundle. `central migrate up` applies the embedded schema under an
  advisory lock.
- **Identity and revocation:** installation certificates carry
  `spiffe://central.iterabase.com/installations/<id>`. Every request is checked
  against an in-memory registry refreshed by `NOTIFY central_registry_changed`
  and a 30 s poll. A registry staler than two minutes refuses everything with
  retryable `upstream_unavailable`.
- **Metering:** one transaction per request writes the usage record, period
  rollup, rate-counter tokens and alert evaluation. If it fails, the response
  is withheld. Credits are exact decimals: 0.01 of the installation's billing
  currency. FX uses the ECB rate locked per calendar month; a missing rate logs
  `fx_rate_unavailable` and refuses retryably.
- **Never stored or logged:** prompts, responses, provider error bodies and the
  provider key.
