# Dependency updates and advisory handling

- **Owner:** repository root
- **Governing config:** [`.github/dependabot.yml`](../.github/dependabot.yml)
- **Last reviewed:** 2026-10-04
- **Tracking:** HOR-608

## Posture

Security updates only, in every ecosystem. Each entry in
`.github/dependabot.yml` sets `open-pull-requests-limit: 0`, which disables
Dependabot *version* updates for that entry; security update pull requests are
not subject to that limit, so advisory-driven pull requests still open. The
repository and organization Advanced Security settings still decide whether
security updates are enabled; the config file steers how they are grouped,
labelled, and targeted.

The per-entry `schedule` is required by the config schema and gates only version
updates, so it is inert while the limit is zero. `cooldown: default-days: 3`
makes the built-in default window explicit.

## Covered ecosystems

| Ecosystem | Directories | Rationale |
| --- | --- | --- |
| `npm` | `control-plane/harness`, `control-plane/ui`, `control-plane/tool-runner`, `control-plane/test/e2e/playwright`, `.github/tools/protobuf` | All five have lockfiles and Dependabot alert coverage. |
| `gomod` | `control-plane`, `inference-gateway`, `forge`, `forge/test/e2e`, `testkit/e2e`, `control-plane/test/e2e`, `charts/test/e2e`, `.github/tools`, `.github/tools/control-plane` | Nine independently buildable modules; a Go bump must keep `make workspace-check` clean. |
| `github-actions` | `/` | Inventory entry only: every external action is SHA-pinned and GitHub Actions alerts require a semantic-version reference, so there is no automated advisory signal. SHA refreshes stay manual. |

Each entry groups only its own directory's security updates
(`applies-to: security-updates`, `patterns: ["*"]`). A config file overrides
repository-level security-update grouping, so configured directories no longer
converge onto one version. The precedent is PR #117: one group moved
`control-plane/harness`, `control-plane/ui`, and `control-plane/tool-runner` to
exactly `vitest 5.0.2` while the advisory's minimum patched version was
`4.1.11` (`GHSA-82fw-gwwq-j7x9`). The harness move had no advisory purpose
(`5.0.0` was outside the vulnerable range) and one failing directory blocked the
other two, which had passed.

The explicit `labels` replace Dependabot's default `dependencies` plus
ecosystem label; the values in the config preserve the repository's existing
`dependencies`, `javascript`, `go`, and `github-actions` labels.

### Deliberately excluded

| Ecosystem | Reason |
| --- | --- |
| `docker` | Dockerfile base-image digests are governed by `.github/inputs/remote-content.json` and enforced bi-directionally by `python3 .github/scripts/remote_content.py validate` in `ci.yml` and `release-candidate.yml`. The `docker` ecosystem has no Dependabot alert coverage, so an entry would open pull requests that fail the authority check with no security gain. |
| `helm`, `docker-compose`, `devcontainers` | Manifests exist but none has Dependabot alert coverage, so entries would be version-update-only, which the posture forbids. |
| `pip`, `cargo`, `bundler`, `terraform`, `deno` | No such manifests exist. |

## Operating conventions

- **Dependency changes are ticket-backed.** A Dependabot pull request is a diff
  source, not a delivery. Adopt its diff on a `<TICKET>-<short-description>`
  branch, keep the Linear identifier in the commits and pull-request title, and
  run required CI and review on that branch.
- **Dependabot pull requests are never self-merged.** Required CI is a floor,
  not an approval; only the user may approve and merge.
- **Vendor-shrinkwrap-pinned advisories are verified on disk, not with
  `npm audit`.** A vendored package can ship an `npm-shrinkwrap.json` that npm
  honours for its subtree (for example `@earendil-works/pi-coding-agent`). A
  root-lockfile edit can then make `npm audit` report clean while the installed
  and shipped copy stays vulnerable; read the on-disk version instead.
- **GitHub Actions SHA refreshes are manual.** GitHub generates Dependabot
  alerts for GitHub Actions only when the action is referenced by a semantic
  version, and all seven external actions here are SHA-pinned, so no alert can
  be raised for them; `actions/cache` is additionally outside the dependency
  graph's scan scope because it is referenced only from composite actions under
  `.github/actions/`. The `github-actions` entry is retained for inventory and
  future semantic-version references only. Routine SHA refreshes are a manual,
  ticket-backed change (HOR-518/HOR-588 precedent), never a scheduled version
  update.
- **Go bumps keep `make workspace-check` clean.** The nine modules are
  independently buildable and share `go.work`; a bump that breaks the workspace
  is not landable (HOR-586 lesson).
- **Dockerfile base-image digests are not Dependabot's.** Adding or changing a
  `FROM reference@sha256:…` requires updating
  `.github/inputs/remote-content.json` in the same change; the
  `remote_content.py validate` check fails on an unlisted or unused authority
  entry.
- **Vendor-pair protection is the harness `tsc` gate.** No `pi-runtime` group
  is configured because a version-update group can govern only version updates,
  which `open-pull-requests-limit: 0` disables; the harness TypeScript gate and
  the equal manifest ranges remain the protection against vendor skew.

## Post-merge verification

HOR-608 acceptance requires observing the first security-update pull request
after this config lands: it must be per-directory and must not converge a
directory onto a version above its minimum patched version. No version-update
pull request may appear in any ecosystem. The observation is recorded on the
ticket before it is accepted.

## Alert dispositions

[`control-plane/docs/moby-test-dependency-risk.md`](../control-plane/docs/moby-test-dependency-risk.md)
records the `github.com/docker/docker` non-reachability decision under HOR-499.
Its four open Dependabot alerts — `GHSA-rg2x-37c3-w2rh` (21),
`GHSA-vp62-88p7-qqf5` (20), `GHSA-x86f-5xw2-fm2r` (19), and
`GHSA-pxq6-2prw-chj9` (17) — were dismissed with reason `not_used` on
2026-10-04 as part of HOR-608. `GHSA-x744-4wpc-v9h2` never produced a
repository alert (`github.com/moby/moby` and `github.com/moby/moby/v2` are its
affected packages, not `github.com/docker/docker`). The re-entry triggers in
that document still apply.
