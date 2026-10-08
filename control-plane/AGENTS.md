# Control-plane instructions

Read the root [`AGENTS.md`](../AGENTS.md) first. Its context, Git, ticket, validation, and architecture-approval rules apply here.

## Scope

Stay inside `control-plane/` for product API, operator/CRDs, dashboard, durable work/runtime, harness, tool gateway, and tool-runner changes. Inspect `charts/` only when a ticket explicitly changes the deployment contract; inspect `inference-gateway/` or `forge/` only for a named cross-component contract.

The module is `github.com/nunocgoncalves/iterabase-mono/control-plane`. Production chart behavior is owned by `charts/`; `config/` is kubebuilder Kustomize for development/envtest.

## Commands

```bash
make build              # Dashboard + manager/api/gateway/dispatch binaries
make test-unit          # short Go tests with race detector
make test               # envtest + integration tests; requires Docker
make lint
make fmt-check
make proto-check        # lint/regenerate protobufs and verify freshness
make ui-test
make harness-test
make harness-isolation-test # required Linux setpriv/UID/filesystem gate; needs Docker
make tool-runner-test
make docker-build       # control-plane image
make test-e2e-unit      # compile the owner E2E suite and run its hermetic tests
make test-e2e-deployed  # deployed-control-plane on fresh Kind; needs the CI-supplied image environment
make test-e2e-execution # deployed-execution-contracts on fresh Kind
```

CI builds every image once per commit and supplies the E2E environment; owner stages never build artifacts. Scenario selection, the environment contract, and the merge-queue rules are in [`../docs/ci.md`](../docs/ci.md).

Run `make proto` after changing `proto/`, and commit all generated Go and TypeScript stubs. Run `make manifests generate` after changing kubebuilder API types and commit generated CRDs/deep-copy code.

## Component boundaries

- `cmd/manager` wires Kubernetes reconcilers; API handlers must not take over operator ownership.
- Postgres is the durable system of record. Keep schema ownership and migrations under `internal/database`.
- `internal/gateway` is the authorized external-effect boundary. Never expose customer credentials to the sandbox or persist raw credential values.
- `internal/dispatch` owns durable worker fencing and assignment semantics; do not add automatic turn execution replay.
- The harness supervisor holds workload credentials and isolation responsibilities; disposable per-turn children must remain outside that trust boundary.
- Artifact bytes remain behind the artifact service and immutable metadata contract.

See [`README.md`](README.md) for the full architecture and directory map. Any change to these boundaries is architectural and requires explicit user approval before implementation.
