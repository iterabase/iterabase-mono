# iterabase-mono

Public monorepo for Iterabase product source.

| Path | Component | Purpose |
| --- | --- | --- |
| [`control-plane/`](control-plane/) | Control plane | Product API, operator, dashboard, harness, and tool runner |
| [`inference-gateway/`](inference-gateway/) | Inference gateway | OpenAI-compatible inference gateway |
| [`forge/`](forge/) | Forge | Host and k3s substrate installer |
| [`charts/`](charts/) | Charts | Helm chart sources |
| [`testkit/e2e/`](testkit/e2e/) | E2E testkit | Shared deterministic test mechanics and compiled catalogue |

Deployment overlays and the marketing site remain separate repositories and are not
part of this monorepo.

## Source authority

This is the sole writable public source for the four product components. Clone
and branch from this repository for every product change:

```bash
git clone https://github.com/iterabase/iterabase-mono.git
cd iterabase-mono
git switch -c HOR-123-short-description origin/master
```

The former standalone component repositories are public read-only archives at
the exact heads imported into this history. Do not open product pull requests or
run releases there. Existing image and chart package names remain unchanged so
overlays continue consuming the same immutable artifact identities. See
[`docs/source-authority.md`](docs/source-authority.md) for the authority audit,
historical evidence, and emergency boundary.

## Go workspace

The committed [`go.work`](go.work) supports atomic changes while preserving independently buildable product, shared-testkit, and owner-suite modules:

| Module directory | Module path |
| --- | --- |
| `control-plane/` | `github.com/nunocgoncalves/iterabase-mono/control-plane` |
| `inference-gateway/` | `github.com/nunocgoncalves/iterabase-mono/inference-gateway` |
| `forge/` | `github.com/nunocgoncalves/iterabase-mono/forge` |
| `forge/test/e2e/` | `github.com/nunocgoncalves/iterabase-mono/forge/test/e2e` |
| `testkit/e2e/` | `github.com/nunocgoncalves/iterabase-mono/testkit/e2e` |
| `control-plane/test/e2e/` | `github.com/nunocgoncalves/iterabase-mono/control-plane/test/e2e` |
| `charts/test/e2e/` | `github.com/nunocgoncalves/iterabase-mono/charts/test/e2e` |

Run the atomic local matrix from the repository root:

```bash
make workspace-check  # go work sync freshness + go list for every module
make build            # production binaries
make test             # module tests + Linux harness isolation; Docker required
make lint
make codegen-check    # protobuf lint/regeneration freshness
make charts-check     # Helm and rendered-resource checks
make testkit-test     # shared race tests + owner examples + compiled catalogue
make e2e-catalogue    # emit registered scenarios as JSON
```

Use `make -C <component> <target>` for a narrower component command. See the
root [`AGENTS.md`](AGENTS.md) ownership map before exploring code and the scoped
`AGENTS.md` in the owning component for prerequisites and architecture rules.

Docker builds retain their runtime identities and use component-scoped contexts:

```bash
make docker-build  # control-plane, harness, isolation, tool-runner, and inference-gateway
```

Required E2E builds each image once per commit and runs the scenarios the
affected-graph selector chooses: Kind scenarios on GitHub-hosted runners and
real-machine scenarios on fresh per-run AWS hosts. Cross-repository
matching-branch checkouts and owner-local artifact builds are not part of
monorepo validation. See [`docs/ci.md`](docs/ci.md).
Forge additionally exposes an explicit, fail-closed workspace purge/reboot
lifecycle for decommissioning; ordinary destroy remains data-preserving.

A merge to `master` does not release. If ticket acceptance requires semantic
publication, the founder publishes after merge through the `release.yml`
workflow documented in [`docs/release.md`](docs/release.md).
