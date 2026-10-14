# Forge instructions

Read the root [`AGENTS.md`](../AGENTS.md) first. Its context, Git, ticket, validation, and architecture-approval rules apply here.

## Scope and commands

`forge` is a Go 1.26 CLI (module `github.com/nunocgoncalves/iterabase-mono/forge`) that bootstraps single-node k3s on a VM/host over SSH, applies the platform release and the locally rendered customer overlay, and provisions hosted AWS environments (`provider.aws`, HOR-593). Keep Forge/bootstrap/substrate behavior in this directory. Charts own declarative platform lifecycle assertions; control-plane owns portable product behavior.

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
- **No local shell-out for host management.** Forge talks to hosts through `golang.org/x/crypto/ssh` and verifies with the remote host's bundled `k3s kubectl`. The operator machine needs no `ssh`, `kubectl`, `helm`, `git`, or `kustomize` (overlays build in-process). Two approved, pinned exceptions exist for `provider.aws` only: the embedded Terraform module runs through `tfexec` with a Terraform binary Forge installs itself (HOR-593 design review), and Forge's SSH stream to hosted instances runs through AWS's session-manager-plugin, downloaded at a pinned, checksum-verified version (`DES-HOR-593-01`). Operators still install nothing. The separate `test/e2e` module may use `client-go` and infrastructure CLIs as test dependencies.
- **Reality as state.** `forge apply` reconciles from the live system, not a persisted authoritative state file. `~/.forge/<install>/` contains only re-fetchable operational artifacts such as kubeconfig, audit logs, and for hosted environments the SSH key and trust file Forge rewrites from the environment's state. The only provisioning record is a hosted environment's Terraform state in the baseline state bucket under `envs/<name>/` (`DES-HOR-603-02`).
- **Idempotency.** Re-apply installs when absent, skips when in sync, refuses immutable cluster/service CIDR or dual-stack changes, and routes k3s version changes through `forge upgrade`.
- **Substrate versus overlay.** `forge.yaml` owns hosts (or `provider.aws`), k3s, node labels, the platform release pin and the overlay path. The overlay owns chart values, object instances and customer tools; do not add overlay toggles to `forge.yaml`.
- **Overlay delivery (`DES-HOR-593-03`, `-04`, `-05`).** Forge renders the overlay on the operator machine and ships only the result: staged value files, owned object instances, and the tools archive as digest-named ConfigMaps. `forge apply` is the only delivery path; no GitOps controller runs in installations and no Git credential enters a cluster. Overlay objects carry `iterabase.com/owner=overlay`; Forge never overwrites or prunes an object without that label, so installation-owned objects (created by Admins or, later, the Chief of Staff) are never touched.
- **Hosted environments (`provider.aws`).** Account-level inputs come only from the SSM contract `/iterabase/hosted/baseline/v1/*` (`DES-HOR-603-03`); every mutation runs as the baseline Forge role. Hosted SSH host keys are generated with the environment and installed by user-data (`DES-HOR-593-02`); trust never comes from the network peer. Teardown order is fixed: LoadBalancer Services, `terraform destroy`, backup prefix, central-gateway certificate, state prefix, then a zero-result `iterabase-env` tag sweep.

## Deferred boundaries

- Product Helm packaging and runtime dependencies are chart-owned, not Forge configuration.
- GPU driver/toolkit/RuntimeClass readiness is substrate behavior; model workload deployment remains control-plane CRD behavior.
- Node labels/taints are install flags; API-based label drift reconciliation is separate scope.
- A Homebrew tap remains deferred.

Any change to these invariants or ownership boundaries is architectural and requires explicit user approval before implementation.
