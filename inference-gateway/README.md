# Inference Gateway

> Canonical source: [`iterabase-mono/inference-gateway`](https://github.com/nunocgoncalves/iterabase-mono/tree/master/inference-gateway). The former standalone repository is historical and read-only.

The Horizonshift/Iterabase inference gateway: an OpenAI-compatible Go service
that routes client requests to vLLM/SGLang backends. It consumes the model
catalog, workload capabilities, and rate limits from the **control-plane**
(directly from the shared Postgres, LISTEN/NOTIFY-synced) and enforces
per-identity rate limits via Redis. Per-customer, self-hosted; pairs with the
control-plane operator, which owns the catalog/auth/policy CRDs.

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
      ──> [ RateLimit: effective identity (Redis) ] ──> same proxy pipeline ──┘
                                │
                  snapshot cache (catalog / capabilities / rate limits)
                                │
            shared Postgres (control-plane schemas)        Redis (rate-limit counters)
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
- **Rate limits:** per-identity RPM/TPM (from the snapshot) enforced via a Redis
  sliding window — the only shared state. TPM incremented from response usage.
- **Redis:** rate-limit counters only.
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
export REDIS_URL="redis://..."          # rate-limit counters
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
redis:        {url: "${REDIS_URL}"}
auth:         {admin_key: "${ADMIN_API_KEY}"}        # /admin/v1/snapshot only
snapshot:
  refresh_interval: 30s       # poll fallback (LISTEN/NOTIFY drives prompt updates)
  readiness_staleness: 60s    # /readyz unhealthy if older than this
logging:      {level: info, format: json}
```

| Env var | Config | |
|---|---|---|
| `DATABASE_URL` | `database.url` | shared Postgres (required) |
| `REDIS_URL` | `redis.url` | rate-limit counters (required) |
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
make build          # bin/gateway
make test           # requires Docker (testcontainers: PG + Redis)
make lint           # golangci-lint
```

```
cmd/gateway/main.go        serve
internal/
  config/                  configuration
  database/                PG connection pool (no migrations — gateway owns no tables)
  snapshot/                control-plane views reader + in-memory LISTEN/NOTIFY cache
  proxy/                   routing + model rewrite + transforms + SSE streaming
  middleware/              customer auth (denied until HOR-513), workload mTLS auth, rate limit, logging, metrics, request ID
  ratelimit/               Redis sliding window (RPM/TPM counters)
  server/                  HTTP server + routes (/v1, /readyz, /admin/v1/snapshot); workload mTLS listener
  spiffe/                  SPIFFE workload-identity parse + mTLS server TLS config (+ testca)
  workload/                live durable-state resolution (pool + active turn scope)
  metrics/                 Prometheus definitions
```
