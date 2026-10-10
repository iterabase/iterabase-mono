# Dependency updates and advisory handling

- **Owner:** repository root
- **Governing config:** [`.github/dependabot.yml`](../.github/dependabot.yml)
- **Last reviewed:** 2026-10-10
- **Tracking:** HOR-608 (security-only steering), HOR-642 (direct bot delivery)

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

**Leave `target-branch` unset, including when the default branch is `master`.**
GitHub's [options reference](https://docs.github.com/en/code-security/dependabot/dependabot-version-updates/configuration-options-for-the-dependabot.yml-file#target-branch)
states that when this option is defined, the entry's options no longer apply to
security updates: those always use the repository default branch. Omitting the
selector lets the per-directory security groups and labels apply. Setting it to
`master` does not add a security-update safeguard; it disables this steering.

`make dependabot-check` guards the implicit default branch, security-only limits,
and one-entry-per-manifest inventory in local checks and the required
`ci-contract` job of `ci.yml`, including full validation. These policy tests are
not proof of GitHub's live behavior; the post-merge observation below remains
mandatory.

## Covered ecosystems

| Ecosystem | Directories | Rationale |
| --- | --- | --- |
| `npm` | `control-plane/harness`, `control-plane/ui`, `control-plane/tool-runner`, `control-plane/test/e2e/playwright`, `.github/tools/protobuf` | All five have lockfiles and Dependabot alert coverage. |
| `gomod` | `control-plane`, `inference-gateway`, `forge`, `forge/test/e2e`, `testkit/e2e`, `control-plane/test/e2e`, `charts/test/e2e`, `.github/tools`, `.github/tools/control-plane` | Nine independently buildable modules; a Go bump must keep `make workspace-check` clean. |
| `github-actions` | `/` | Inventory entry only: every external action is SHA-pinned and GitHub Actions alerts require a semantic-version reference, so there is no automated advisory signal. SHA refreshes stay manual. |

Each entry declares a group for only its own directory's security updates
(`applies-to: security-updates`, `patterns: ["*"]`). These rules override
repository-level security-update grouping only when the entry applies to
security updates (in particular, `target-branch` must be absent). Per-directory
behavior must be confirmed after merge, not inferred from valid YAML. The
precedent is PR #117: one group moved
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
| `docker` | Dockerfile base-image digests are governed by `.github/inputs/remote-content.json` and enforced bi-directionally by `python3 .github/scripts/remote_content.py validate`, which `make release-check` runs in the `ci-contract` job of `ci.yml`. The `docker` ecosystem has no Dependabot alert coverage, so an entry would open pull requests that fail the authority check with no security gain. |
| `helm`, `docker-compose`, `devcontainers` | Manifests exist but none has Dependabot alert coverage, so entries would be version-update-only, which the posture forbids. |
| `pip`, `cargo`, `bundler`, `terraform`, `deno` | No such manifests exist. |

## Operating conventions

- **Dependency changes are ticket-backed.** Security-only Dependabot pull
  requests may be reviewed directly; copying an ordinary security patch onto a
  human branch is not required (DES-HOR-642-02). Link the delivery ticket, use a
  `<TICKET> — <description>` PR title, and supplement the generated notes with
  the normal `Summary`, `Validation`, `Production impact`, and `Ticket state`
  sections before review. Generated bot branch/commit names are the only naming
  exception; required CI, reviewer-owned terminal review, acceptance, and release
  gates still apply. For authored fixes, major-version migrations, or updates
  the bot cannot safely deliver (including vendor-shrinkwrap constraints), use
  the normal `<TICKET>-<short-description>` branch and ticket-prefixed commits.
- **Dependabot pull requests are never self-merged or auto-merged.** Required CI
  is a floor, not an approval; only the user may approve and merge. Classify
  semantic publication on every delivery as required, deferred, or none. A
  dev-only tooling fix can require no dedicated release; a shipped dependency
  fix reaches installations only through the approved publication/deployment
  path. Merge is not publication.
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

The first observation after PR #127 merged failed: [PR #132](https://github.com/iterabase/iterabase-mono/pull/132)
(2026-10-06), then its replacement [PR #135](https://github.com/iterabase/iterabase-mono/pull/135)
(2026-10-09), grouped harness, tool-runner, and UI into one security PR.
`source-map-js` moved from 1.2.1 to the advisory minimum 1.2.2, so the failed
condition was directory isolation, not the version floor. [Update run 37430624324](https://github.com/iterabase/iterabase-mono/actions/runs/37430624324)
had `security-updates-only: true`, the fallback `npm_and_yarn` group, and all
three source directories rather than the configured `security` group. The
original `target-branch: master` on every entry made those options inapplicable
to security updates. This remediation removes the selector; it does not waive
the failed observation or change repository/organization security settings.

Before accepting HOR-608 after the remediation merges:

1. Record the remediation merge SHA and the subsequent Dependabot run/PR URLs
   on the ticket. Confirm that new runs use a source revision containing the
   fix, one configured directory per job, and the configured `security` group
   rather than the cross-directory fallback `npm_and_yarn` group. A rebase of
   an older PR is not proof that a new job used the corrected configuration.
2. Inspect the first new security-update PR produced with the corrected config:
   it must be per-directory and must not converge a component onto a version
   above its minimum patched version. Compare the actual changed manifest paths
   and versions with each advisory's patched floor, not just the PR title.
3. Observe that no version-update PR appears in any ecosystem over the first
   weeks after merge; record the observation dates and results on the ticket.

Criteria 2 and 4 remain open until this evidence exists (or an explicit founder
exception is recorded). Valid YAML, passing CI, or no trigger alone does not
satisfy the security-PR observation. The four Moby dismissals remain valid; this
config remediation does not remediate the remaining package advisories.

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
