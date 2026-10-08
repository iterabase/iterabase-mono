# Forge instructions

Read the root [`AGENTS.md`](../AGENTS.md) first. Its context, Git, ticket, validation, and architecture-approval rules apply here.

## Scope and commands

`forge` is a Go 1.26 CLI (module `github.com/nunocgoncalves/iterabase-mono/forge`) that bootstraps single-node k3s on a VM/host over SSH. Keep Forge/bootstrap/substrate behavior in this directory. Charts own declarative platform lifecycle assertions; control-plane owns portable product behavior.

```bash
make build          # -> bin/forge
make test           # unit + fake-SSH integration tests
make test-e2e           # forge/cpu on a fresh CPU host; needs the fixture environment
make test-e2e-workspace # forge/cpu-workspace on a fresh CPU host
make test-e2e-gpu       # forge/gpu on a fresh GPU host
make test-e2e-unit      # compile/test the nested E2E module without infrastructure
make lint
make fmt-check
```

The separate E2E module is `github.com/nunocgoncalves/iterabase-mono/forge/test/e2e`. Monorepo source-composed runs use local `../control-plane` and `../charts` directories; do not coordinate matching branches or check out those legacy repositories.

The F3 targets run against a host CI launches for that one run: a fresh AWS EC2 instance booted from the baked fixture AMI (C1, `DES-HOR-590-03`), reached over SSH with a host key pinned at launch, and terminated afterwards. `aws_ci.py launch-fixture` exports the host contract (`FORGE_E2E_FIXTURE*`, `FORGE_E2E_IMAGE_CACHE_*`, and on GPU `FORGE_E2E_MODEL_CACHE_*`) and `e2e_inputs.py prepare` exports the artifacts (`FORGE_E2E_BINARY`, chart and image archives). There are no permanent fixtures. See [`../docs/ci.md`](../docs/ci.md#scenario-environment) and [`test/e2e/DESIGN.md`](test/e2e/DESIGN.md).

## Architecture invariants

- **`Provisioner` interface seam.** Lifecycle/reconcile logic in `internal/lifecycle` orchestrates against `internal/provisioner.Provisioner`. The real SSH implementation is `internal/sshprovisioner`. Lifecycle logic must never call SSH directly.
- **No local shell-out for host management.** Forge talks to hosts through `golang.org/x/crypto/ssh` and verifies with the remote host's bundled `k3s kubectl`. The operator machine needs no `ssh`, `kubectl`, or `helm`. The separate `test/e2e` module may use `client-go` and infrastructure CLIs as test dependencies.
- **Reality as state.** `forge apply` reconciles from the live system, not a persisted authoritative state file. `~/.forge/<install>/` contains only re-fetchable operational artifacts such as kubeconfig and audit logs.
- **Idempotency.** Re-apply installs when absent, skips when in sync, refuses immutable cluster/service CIDR or dual-stack changes, and routes k3s version changes through `forge upgrade`.
- **Substrate versus overlay.** `forge.yaml` owns hosts, k3s, and node labels. Product/client overlays own chart values and CRDs; do not add overlay toggles to `forge.yaml`.

## Deferred boundaries

- Product Helm packaging and runtime dependencies are chart-owned, not Forge configuration.
- GPU driver/toolkit/RuntimeClass readiness is substrate behavior; model workload deployment remains control-plane CRD behavior.
- Node labels/taints are install flags; API-based label drift reconciliation is separate scope.
- A Homebrew tap remains deferred.

Any change to these invariants or ownership boundaries is architectural and requires explicit user approval before implementation.
