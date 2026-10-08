# Iterabase monorepo operating instructions

## Bound context before exploring

- Start with this file, identify the owning directory below, then read that directory's `AGENTS.md` before inspecting implementation files.
- Do not search or read the whole repository by default. Keep exploration inside the owning directory unless the ticket names a cross-component contract or evidence proves another owner must change.
- For a cross-component change, inspect only the named contract surfaces and record why each additional directory is in scope.
- Deployment overlays and the marketing site are separate repositories. Do not look for or add customer-specific overlay behavior here.

## Ownership map

| Directory | Owns | Scoped instructions |
| --- | --- | --- |
| `control-plane/` | Product API, operator/CRDs, dashboard, durable work runtime, harness, and tool runner | [`control-plane/AGENTS.md`](control-plane/AGENTS.md) |
| `inference-gateway/` | OpenAI-compatible inference routing, snapshot consumption, auth enforcement, and rate limiting | [`inference-gateway/AGENTS.md`](inference-gateway/AGENTS.md) |
| `forge/` | Host/k3s bootstrap, substrate reconciliation, and Forge-owned E2E | [`forge/AGENTS.md`](forge/AGENTS.md) |
| `charts/` | Helm packaging and declarative install/upgrade/rollback behavior | [`charts/AGENTS.md`](charts/AGENTS.md) |
| `testkit/` | Shared deterministic E2E mechanics and compiled scenario catalogue | [`testkit/AGENTS.md`](testkit/AGENTS.md) |
| `docs/` | Monorepo-wide durable repository documentation | This file |

The Go modules remain independently buildable. The root `go.work` is for atomic local development; do not merge component modules or introduce cross-module imports without an approved architectural decision.

## Source and release authority

- This monorepo is the sole writable public source for the product components above. The former `control-plane`, `inference-gateway`, `forge`, and `iterabase-charts` repositories are historical archives; never target them for changes, pull requests, CI, or releases.
- Official artifacts publish to `ghcr.io/iterabase/*`, with charts under the `ghcr.io/iterabase/iterabase-charts` OCI namespace. These are stable artifact identities, not source repository links. Do not rename them during source maintenance. Artifacts published earlier under `ghcr.io/nunocgoncalves/*` remain published and are never deleted or republished.
- A merge to `master` is integration, not a semantic release. Ticket acceptance must state whether publication is required (required, deferred, or none). When it is, publish only through the manual, founder-approved `release.yml` workflow described in [`docs/release.md`](docs/release.md); never publish implicitly from merge or acceptance.
- Deployment overlays continue to reconcile independently against immutable published artifacts. Do not couple overlay changes to a source ticket unless the ticket explicitly names that external contract.

See [`docs/source-authority.md`](docs/source-authority.md) for the cutover audit and catastrophic-only unarchive boundary.

## Dependency and supply-chain updates

- Dependabot is configured security-updates-only in [`.github/dependabot.yml`](.github/dependabot.yml). Every entry sets `open-pull-requests-limit: 0`, which disables version updates for that ecosystem while security-update pull requests remain exempt. Version updates, auto-merge, and `docker`, `helm`, `docker-compose`, or `devcontainers` entries require a new recorded decision.
- Dependency pull requests are ticket-backed and are never self-merged. A Dependabot pull request is a diff source; only the user approves and merges, and required CI is a floor, not an approval.
- Security updates are grouped per configured directory. Do not add `group-by: dependency-name`, repository-level, or organization-level grouping that can converge components onto a version above the minimum patched version.
- GitHub Actions SHAs are refreshed manually in a ticket-backed change, never by a scheduled version-update pull request.
- Advisory disposition uses reachability evidence. Verify vendor-shrinkwrap-pinned packages against the installed and shipped on-disk version, not `npm audit`; dismiss untriggered advisories with the documented reason and record the evidence. Dismissed Moby daemon advisories and their re-entry triggers live in [`control-plane/docs/moby-test-dependency-risk.md`](control-plane/docs/moby-test-dependency-risk.md).
- Base-image and Dockerfile-frontend digests are governed by [`.github/inputs/remote-content.json`](.github/inputs/remote-content.json) and `.github/scripts/remote_content.py validate`, not by Dependabot.
- Coverage, exclusions, and the conventions for dependency pull requests are recorded in [`docs/dependencies.md`](docs/dependencies.md).

## Shared ticket and Git workflow

- Direct pushes to `master` are prohibited. Work on one `<TICKET>-<short-description>` branch.
- Branch names, commit messages, and pull request titles must include the Linear identifier, for example `HOR-123-short-description`, `HOR-123: describe change`, and `HOR-123 — Describe change`.
- Keep commits coherent and limited to the ticket. Do not mix unrelated component cleanup into an atomic change.
- Open a pull request when validation is complete; only the user may approve and merge it.
- Pull request bodies use `## Summary`, `## Validation`, `## Production impact`, and `## Ticket state`, with real Markdown line breaks and `None`/`N/A` where appropriate.
- After pushing, watch required CI to completion. A review is not addressable-complete and a ticket is not complete while required CI is failing.
- The required checks are `CI / required` and `E2E / required`. Pull requests run the deterministic affected-graph selection; the merge queue re-runs it strictly on the exact merge commit. See [`docs/ci.md`](docs/ci.md).
- A change to CI (`.github/**`, `testkit/**`, `release/**`, the root `Makefile`, `go.work`) must also pass full validation at the pull request's head before review is requested: add the `full-validation` label. Add the same label whenever the user or the ticket asks for full validation. The label triggers one run when it is added; remove and re-add it to validate a later head.
- Add the `e2e-real-machine` label to run the real-machine (F3) scenarios strictly on the pull request for AgentPool, LVM, storage, or bootstrap work, instead of first meeting them in the merge queue.
- Publication is never part of a pull request. It happens after merge, only through `release.yml`, only with founder approval in the protected `release` environment. Bump versions with `make bump TARGET=<target> VERSION=<x.y.z>` in the ticket that needs the release.
- The repository is the source of truth for non-secret infrastructure intent and architecture. Linear is the source of truth for ticket state, ownership, sequencing, and completion.

## Architecture decisions

Architectural decisions require explicit user approval before implementation, even when they seem consistent with existing guidance. This includes cross-service contracts, datastore/cache/transport choices, failure or isolation models, and patterns future tickets would inherit. Surface ambiguity instead of choosing unilaterally.

## Root validation and builds

Use root targets for atomic checks and component targets while working inside one owner:

```bash
make workspace-check  # go work sync freshness + every module's go list
make build            # production Go binaries, preserving component outputs
make test             # component tests + required Linux harness isolation + Forge E2E harness tests
make lint             # all Go modules, including Forge E2E
make codegen-check    # protobuf freshness
make charts-check     # Helm/static chart validation
make release-check    # selector, release plan, version bump, AWS substrate, remote-content and cache contracts
make release-security-audit # authenticated GitHub environment, deploy-key, tag-ruleset, and workflow-permission audit
make bump TARGET=<target> VERSION=<x.y.z> # move every version field linked to one release target
make docker-build     # control-plane and inference-gateway images
make check            # complete local matrix above
make install-hooks    # shared monorepo pre-commit hook
```

Component-specific prerequisites and narrower commands live in each component's `AGENTS.md` and `README.md`.
