# Charts instructions

Read the root [`AGENTS.md`](../AGENTS.md) first. Its context, Git, ticket, validation, and architecture-approval rules apply here.

## Scope

Stay inside `charts/` for Helm packaging and declarative install, upgrade, feature-enable, reapply, rollback, TLS, Service, persistence, and rollout behavior. Runtime product behavior belongs to `control-plane/` or `inference-gateway/`; host/bootstrap behavior belongs to `forge/`.

## Development

- Charts live under `charts/<chart-name>/`; `charts/iterabase-platform/` is the umbrella.
- Local prerequisites are `helm` and `kubeconform`.
- Run `make check` before handoff. It builds dependencies, lints every chart, templates the umbrella and control-plane chart, validates rendered resources, and runs repository contract scripts.
- Use `make check-tls` and `make check-observability` for those non-default value paths.
- `make build-deps` vendors local `file://` dependencies plus external dependencies into gitignored chart directories.
- Build local source from this monorepo directory; do not check out `control-plane`, Forge, or other product source repositories beside it.
- Runtime E2E: `make test-e2e-unit` (no cluster), `make test-e2e-install`, `make test-e2e-n-1-upgrade`, `make test-e2e-observability`, and `make test-e2e-observability-tls`. Each runnable scenario declares its `renders`, so the CI selector picks it only when a chart change alters what it installs ([`../docs/ci.md`](../docs/ci.md)).
- `make check` includes the C7 version-link lint: component chart `appVersion` equals `<component>/VERSION` and both substrate versions equal the platform version. Use `make bump TARGET=<target> VERSION=<x.y.z>` from the repository root.

## Chart invariants

- Every subchart wraps resources in `{{- if .Values.enabled }}`; the umbrella controls it through `condition: <chart>.enabled`.
- Chart-generated secrets must remain stable across upgrades through the Helm `lookup` pattern. Never commit real secrets.
- Runtime image, chart, release, Service, and configuration names are external contracts and must not change during source/module migrations.
- Per-chart tags remain `<chart>-<semver>` and publish to `oci://ghcr.io/iterabase/iterabase-charts/<chart>` according to the approved release contract; data dependency subcharts remain bundled only.

Any change to ownership, release identity, secret lifecycle, or install/upgrade semantics is architectural and requires explicit user approval before implementation.
