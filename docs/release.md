# Official releases

Authority: C7, C8, and C10 of the CI/CD overhaul (HOR-590, approved
2026-10-08). CI, previews, and full validation are in [`ci.md`](ci.md).

`master` is an integration branch, not a publication trigger. The only
publication path is the manual `release.yml` workflow, approved by the founder in
the protected `release` environment. No merge, push, tag push, preview, or
acceptance step publishes a semantic artifact.

## Ticket acceptance and release intent

Every ticket classifies semantic publication as:

- **Required for ticket acceptance:** after merge, the founder selects the
  release targets for an exact `master` SHA; the successful `release.yml` run,
  its attested Releases, and any named-environment deployment evidence block
  Done.
- **Deferred to product release review:** engineering can be accepted without
  publication; the product release gate later authorizes the target set.
- **None:** no semantic artifact is required.

Path selection may inform a proposal but never chooses release intent. A preview
link may be cited as live verification evidence; it is not a release.

## Independently versioned targets (C7)

Targets keep independent versions and independent releases.
`release/targets.json` defines each target, its tag prefix, and its artifact
recipes.

| Target | Version authority | Tag | Published outputs |
| --- | --- | --- | --- |
| `control-plane` | `control-plane/VERSION` | `control-plane-v<version>` | control-plane, harness, and tool-runner images |
| `inference-gateway` | `inference-gateway/VERSION` | `inference-gateway-v<version>` | inference-gateway image |
| `forge` | `forge/VERSION` | `forge-v<version>` | Linux/macOS × amd64/arm64 archives and checksums |
| `control-plane-chart` | `charts/charts/control-plane/Chart.yaml` `version` | `control-plane-<version>` | control-plane OCI chart |
| `inference-gateway-chart` | `charts/charts/inference-gateway/Chart.yaml` `version` | `inference-gateway-<version>` | inference-gateway OCI chart |
| `iterabase-platform-chart` | `charts/charts/iterabase-platform/Chart.yaml` `version` | `iterabase-platform-<version>` | platform chart plus the same-version `cert-manager-substrate` and `lvm-storage-substrate` companions |

Images publish to `ghcr.io/iterabase/<image>:<version>` and charts to
`oci://ghcr.io/iterabase/iterabase-charts/<chart>`. Artifacts published earlier
under `ghcr.io/nunocgoncalves/*` stay published.

### The source tree pins the composition

The composition of a release comes from the source tree at the release SHA:

- the platform chart bundles the component charts through `file://`
  dependencies at the versions its `Chart.yaml` names;
- each component chart's image tags default to its `appVersion`;
- each component chart's `appVersion` equals its component `VERSION` file;
- both substrate companions carry the platform chart's version, because Forge
  resolves them at that version.

`charts/scripts/check-version-links.sh` enforces the last three links. It runs in
`make -C charts check` (the `charts-static` CI job) and fails when an
`appVersion` differs from `<component>/VERSION`, when a rendered
control-plane, tool-runner, or inference-gateway image tag differs from the
`appVersion`, or when a substrate version differs from the platform version.

### Bumping a version

`make bump TARGET=<target> VERSION=<x.y.z>` (`release/bump.py`) moves every
field linked to that target in one change:

| Target | Fields moved |
| --- | --- |
| `control-plane`, `inference-gateway` | `<component>/VERSION` and the component chart `appVersion` |
| `forge` | `forge/VERSION` |
| `control-plane-chart`, `inference-gateway-chart` | the chart `version` and the platform chart's dependency version |
| `iterabase-platform-chart` | the platform `version` and `appVersion`, and both substrate versions |

Bumping a component's image version therefore also needs a chart bump (and a
platform bump) for that version to ship in the platform chart. A version-only
pull request runs the install-readiness smoke (selector rule 3).

## Release workflow (`release.yml`, C10)

Dispatch `release.yml` with:

- `sha` — a full commit SHA on `master`;
- `targets` — a comma-separated, non-empty target set.

```bash
gh workflow run release.yml --repo iterabase/iterabase-mono --ref master \
  -f sha=<full-sha> -f targets=control-plane,control-plane-chart,iterabase-platform-chart
```

The workflow runs in one `release` concurrency group and never cancels a run in
progress.

1. **Plan.** It requires a 40-character SHA that is an ancestor of `master`, and
   `CI / required` and `E2E / required` green at that SHA. `release/release_plan.py`
   reads each target's version at the SHA and the published tags, and then:
   - fails when any member's tag is already published (bump it first);
   - fails when a member references a version of another target that is neither
     published nor released in the same set, and names that target. A component
     chart references its component's `appVersion`; the platform chart references
     both component chart versions. The set is never expanded silently.
2. **Full validation** (`full-validation.yml`) of the exact release
   composition: targets being released use the `sha-<sha>` builds, and every
   other referenced image uses its already-published official version.
3. **Founder approval** in the protected `release` environment.
4. **Publish**, without rebuilding anything that was tested:
   - images: `crane copy` of the tested
     `ghcr.io/iterabase/preview/<image>:sha-<sha>` digest to
     `ghcr.io/iterabase/<image>:<version>`, then a check that the promoted digest
     equals the tested digest (C8);
   - an `actions/attest-build-provenance` attestation per image digest, pushed to
     the registry;
   - charts: `helm package` at the release SHA as-is, with no version rewriting,
     pushed to `oci://ghcr.io/iterabase/iterabase-charts`;
   - Forge: built by GoReleaser from `forge/.goreleaser.yaml` at the release SHA;
   - one attestation over the chart and Forge archives;
   - annotated namespaced tags at the SHA, pushed with the `RELEASE_TAG_SSH_KEY`
     deploy key held by the `release` environment;
   - one immutable GitHub Release per target with its archives attached.

Images embed the version from the target's `VERSION` file at that SHA plus the
commit (C8). The embedded version only appears in startup logs, `build_info`
metric labels, and `forge version`/audit records, so promoting a tested digest
under its version tag is safe. A preview image reports the upcoming version plus
its commit; the preview tag and commit distinguish it.

### Latest

GitHub's repository-wide Latest marks the most recent `iterabase-platform-chart`
Release; every other target publishes with `--latest=false`. `charts/n-1-upgrade`
installs the newest published platform-chart Release as its N-1 baseline.

### Release evidence

Ticket acceptance cites the `release.yml` run, the attested GitHub Release per
target, and the promoted image digests, which the run summary shows identical to
the tested `sha-<sha>` digests.

## Fix forward

A broken release is fixed forward with a patch release. There is no rollback
workflow.

Rolling an installation back with `forge apply` of the previous version is
possible only when no irreversible change is involved. The control-plane init
container only runs `migrate up`: an older image over a newer schema fails unless
`control-plane-api migrate down` is first run manually with the newer image,
which can lose data. A release that contains a schema migration is therefore
fix-forward only.

## Resuming a failed publish

If `publish` fails after some artifacts are already in the official registry,
dispatch `release.yml` again for the same SHA and targets. The plan still
passes, because no tag was pushed. Full validation runs again, then you approve
again. `publish` then picks up without changing anything already published:

- an image version that already holds the tested digest is kept; any other
  digest fails and needs a fix forward;
- a chart version that is already in the registry is pulled back and attested
  as published, never packaged and pushed again (Helm archives are not
  byte-reproducible);
- Forge archives, attestations, tags and Releases are then produced as normal.

## Protection and audit

The `release` environment requires founder review, allows only `master`, and
holds `RELEASE_TAG_SSH_KEY`. The tag ruleset protects the namespaced release
tags, and the release deploy key is the repository's only write deploy key.
Immutable Releases stay enabled.

`make release-security-audit` (`.github/scripts/audit_release_security.sh`)
verifies, for `iterabase/iterabase-mono`:

- the `release` environment protection, its `master`-only branch policy, and
  its deploy-key identity;
- the active release-tag ruleset and its bypass authority;
- the immutable-releases setting (admin-authenticated runs only);
- that the writer set is exactly the founder;
- that no permanent-fixture (`FORGE_E2E_*`) secret or credential-shaped
  variable remains;
- that only `release.yml` uses the `release` environment and writes repository
  contents, that only `e2e.yml`, `full-validation.yml`, and `release.yml` write
  packages, and that no workflow uses `pull_request_target`.

Run it after any change to the environment, rulesets, deploy keys, or workflow
permissions.
