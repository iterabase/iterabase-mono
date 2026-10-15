# Inference-gateway instructions

Read the root [`AGENTS.md`](../AGENTS.md) first. Its context, Git, ticket, validation, and architecture-approval rules apply here.

Stay inside `inference-gateway/` for OpenAI-compatible inference routing, snapshot consumption, customer/workload authentication, Postgres rate limiting, backend proxy behavior, and the central gateway (`cmd/central`). Inspect `control-plane/` only for an explicitly named shared database or workload contract; deployment packaging remains owned by `charts/`.

## Project Overview

Go (1.26) module with two binaries in one image:

- `cmd/gateway`: the installation's OpenAI-compatible gateway. It routes
  `/v1/chat/completions` and `/v1/models` from the control-plane catalog
  snapshot to vLLM/SGLang, keyed external (BYO-key) providers, or the central
  gateway for `managed` models. It covers workload mTLS auth, Postgres rate
  limits and SSE streaming.
- `cmd/central`: the central Iterabase gateway (HOR-661). It provides
  installation mTLS, the KMS-backed installation CA, the registry and
  entitlements, authoritative credit metering, FX, alerts, the operator API and
  the invoicing export. Its contract is
  [`docs/architecture/v2-central-inference-gateway.md`](../docs/architecture/v2-central-inference-gateway.md).

Key stack: Go stdlib + chi v5, PostgreSQL (pgx v5), Prometheus, aws-sdk-go-v2
(KMS, SNS) for central, and testcontainers-go for integration tests. There is
no Redis (DES-HOR-661-07).

## Build / Lint / Test Commands

```bash
make build                          # -> bin/gateway, bin/central
make lint                           # golangci-lint run ./...
make fmt                            # go fmt ./... (+ goimports -w if installed)
make fmt-check                      # fail if gofmt/goimports would change files
make install-hooks                  # git config core.hooksPath .githooks
make test                           # go test -v -race -count=1 ./...

# One test / one package
go test -v -race -count=1 -run TestCentral_FailsClosed ./internal/central/server/
go test -v -race -count=1 ./internal/central/billing/

docker compose up --build           # postgres + installation gateway
```

**Important:** integration tests use testcontainers-go (`internal/testpg`) and
need Docker. They run real PostgreSQL, with no mocks for infrastructure.
`-short` skips them.

## Project Structure

```
cmd/gateway/main.go          Installation gateway: serve
cmd/central/main.go          Central gateway: serve | migrate up | ca init
internal/
  attribution/               Payload-free installation -> central attribution headers
  centrallink/               Installation mTLS client to central (certificate reload)
  central/
    billing/                 Credit policy as pure functions (amounts, FX, periods, alerts)
    config/                  Central config (CENTRAL_CONFIG YAML + env)
    fx/                      ECB reference-rate fetcher
    notify/                  SNS alert publisher + retry loop
    opauth/                  Operator auth: presigned sts:GetCallerIdentity + allow-list
    pki/                     KMS signer, CA, installation + serving certificates
    registry/                In-memory registry (LISTEN/NOTIFY, fail closed when stale)
    server/                  Central HTTP: inference, installation read, operator API, health
    store/                   Central Postgres (schema central, embedded migrations)
  config/                    Installation gateway YAML + env config
  database/                  pgx pool (the installation gateway owns no tables)
  metrics/                   Prometheus metric definitions
  middleware/                Auth, workload mTLS auth, logging, metrics, rate limit, request-id
  proxy/                     Installation handler + the shared Forwarder (round trip, SSE,
                             error classification, usage extraction) used by both binaries
  ratelimit/                 Postgres one-minute counters + stream TPM batcher
  server/                    Installation gateway HTTP server + chi route wiring
  snapshot/                  Control-plane catalog/permissions views + LISTEN/NOTIFY cache
  spiffe/                    Workload SPIFFE identity parsing (+ testca)
  testpg/                    Testcontainers Postgres helper (tests only)
  upstreamauth/              Kubernetes Secret resolver for keyed external backends
  workload/                  Live durable turn-scope resolution
```

## Invariants

- **No payload anywhere.** Prompts, responses and provider error bodies are
  never logged, stored, put in metrics or forwarded to central's records.
  `TestCentral_NoPayloadLeaks` enforces this.
- **Central never serves unmetered inference.** A failed usage write withholds
  the response (`proxy.Hooks.Settle`). Limits, registry and FX checks fail
  closed (DES-HOR-661-08).
- **One Forwarder.** Both binaries forward through `proxy.Forwarder`; do not
  fork the round trip, SSE handling or error classification.
- **Credits are exact.** Use `math/big.Rat` (`central/billing`), never floats.

## Code Style Guidelines

### Formatting

- Standard `gofmt` / `goimports` formatting. No custom formatter config.
- golangci-lint configured via `.golangci.yml` (consistent with the `forge` repo).
- Line length: no strict limit, but keep lines reasonable (~100-120 chars).

### Imports

Three groups separated by blank lines, in this order:
1. Standard library (`fmt`, `net/http`, `log/slog`, etc.)
2. Third-party (`github.com/go-chi/chi/v5`, `github.com/jackc/pgx/v5`, etc.)
3. Internal (`github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/...`)

When aliasing imports, use short descriptive names:
```go
chimiddleware "github.com/go-chi/chi/v5/middleware"
gatewaymw     "github.com/nunocgoncalves/iterabase-mono/inference-gateway/internal/middleware"
```

### Types and Naming

- **Exported types**: PascalCase with doc comments (`// Handler is the main proxy handler.`).
- **Unexported types**: camelCase, used for internal state (e.g., `backendState`, `chatCompletionRequest`).
- **Constructors**: `NewXxx(deps...) *Xxx` pattern. Accept interfaces, return concrete types.
- **Sentinel errors**: package-level `var ErrXxx = errors.New(...)` (e.g., `ErrNoBackends`, `ErrAllUnhealthy`).
- **JSON struct tags**: snake_case with `omitempty` for optional fields.
- **Pointer fields for "not set" semantics**: Use `*float64`, `*int`, `*bool` when nil must be distinguished from zero value (see `DefaultParams`, `ReasoningConfig`).

### Error Handling

- Return errors up the call stack; let the handler decide the HTTP response.
- HTTP errors use OpenAI-compatible JSON format everywhere:
  ```json
  {"error": {"message": "...", "type": "invalid_request_error", "code": "..."}}
  ```
- Use `writeError(w, status, message, code)` helpers — don't write raw errors.
- Log errors with structured logging: `h.logger.Error("msg", "key", value)`.
- Provider failures use the classified `upstream_*` codes in `internal/proxy/upstream.go` (DES-HOR-597-02); never pass a provider's error body or credential through.

### Middleware Pattern

Chi-compatible middleware: `func(http.Handler) http.Handler`. Use context for
passing data between middleware and handlers (`auth.WithAPIKey`, `middleware.SetMetricsData`,
`middleware.RequestIDFromContext`).

### Logging

Use `log/slog` structured logger everywhere. Pass `*slog.Logger` via dependency injection.
Log with key-value pairs: `logger.Error("backend request failed", "backend", url, "error", err)`.

### Concurrency

- Protect shared state with `sync.Mutex` (see `WeightedRoundRobin`).
- Use `-race` flag in all test runs.
- Background goroutines (health checker, cache refresh, TPM batcher) accept a
  `context.Context` and stop on cancellation.

### Testing Patterns

- Use `github.com/stretchr/testify` for assertions (`assert`, `require`).
- `require` for fatal checks (setup, nil errors), `assert` for non-fatal.
- Integration tests: use `testcontainers-go` for real PostgreSQL (`testpg.Start(t)`).
- Unit tests: use `httptest.NewServer` for mock HTTP backends.
- Test function naming: `TestMethodName_Scenario` (e.g., `TestChatCompletions_NonStreaming`).
- Use `t.Setenv()` for env vars, `t.TempDir()` for temp files.
- Tests use `-count=1` to disable caching.

### Dependency Injection

Constructor-based DI. The `server.Deps` struct aggregates all dependencies.
`cmd/gateway/main.go` wires everything together. No DI frameworks.

### Configuration

- YAML config file (`gateway.yaml`) with `os.ExpandEnv` for variable substitution.
- Env vars override YAML values (e.g., `DATABASE_URL` overrides `database.url`). Central reads only the environment (`CENTRAL_*`).
- Validation in `config.Validate()` — required fields, sensible defaults.

### Database

- PostgreSQL via pgx v5 connection pool.
- The installation gateway owns no tables; the control-plane migrations create what it reads and writes.
- Central owns schema `central`: embedded SQL in `internal/central/store/migrations/`, applied by `central migrate up` under an advisory lock.
- JSONB columns for flexible nested config (DefaultParams, ReasoningConfig, etc.)
  with custom `Scan` methods.

### HTTP Responses

- Always set `Content-Type: application/json`.
- Use `json.NewEncoder(w).Encode(...)` for responses.
- Propagate `X-Request-ID` header across proxy requests.
- Rate limit headers: `X-RateLimit-Limit-RPM`, `X-RateLimit-Remaining-RPM`, etc.
