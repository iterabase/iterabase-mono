# Monorepo continuous integration

Authority: the CI/CD overhaul decisions C1–C13 and `DES-HOR-590-01`..`04`
(HOR-590, approved 2026-10-08), on top of the HOR-591 AWS substrate
(`DES-HOR-591-01/02`, [`runbooks/aws-ci.md`](runbooks/aws-ci.md)). Release
publication is in [`release.md`](release.md); test placement and tiers are in
[`testing/STRATEGY.md`](testing/STRATEGY.md).

The two required checks are:

- `CI / required`
- `E2E / required`

Both run on every pull request and on every merge-queue commit. Each passes only
when the selection succeeded, every selected job succeeded, and every unselected
job was skipped. A canceled, failed, or unexpectedly run job fails the aggregate.

## Workflows

| Workflow | Trigger | Does |
| --- | --- | --- |
| `ci.yml` | pull request, `merge_group`, push to `master`, manual, `workflow_call` | selector; lint, type, unit, integration and static checks for the selected owners; `CI / required` |
| `e2e.yml` | pull request, `merge_group`, push to `master`, `workflow_call` | selector; build every image once; selected Kind and real-machine scenarios; the non-required preview deploy; `E2E / required` |
| `preview.yml` | pull request closed | tears down the `pr-<N>` preview |
| `full-validation.yml` | nightly at 02:17 UTC when `master` changed; manual; `full-validation` label; `workflow_call` from `release.yml` | the whole catalogue against one commit (C11) |
| `release.yml` | manual, protected `release` environment | official release ([`release.md`](release.md)) |
| `bake.yml` | push to `master` changing `.github/inputs/remote-content.json` or `forge/test/e2e/model-cache.json`; manual; `workflow_call` from `e2e.yml` | fixture AMIs and the model-cache snapshot (C1, `DES-HOR-590-03`) |
| `reaper.yml` | hourly at minute 17; manual | expired CI instances and old baked generations (C1, C12) |
| `aws-ci-smoke.yml` | manual only | the HOR-591 substrate proof |

`ci.yml` and `e2e.yml` cancel superseded runs per pull request (or per
merge-queue entry). Full-validation runs use a separate concurrency group, so a
labeled run does not cancel the ordinary pull-request run.

## The affected-graph selector (C2)

`.github/actions/select` compiles the scenario catalogue
(`testkit/e2e/cmd/e2e-catalogue`) and runs `.github/scripts/affected.py`. Both
workflows use it, so CI jobs and E2E scenarios come from one selection. The
selection (jobs, artifacts, scenarios, optional stages, classification, and the
reason for each choice) is written to the run summary and uploaded as the
`e2e-selection` artifact.

| Event | Diff | Mode |
| --- | --- | --- |
| `pull_request` | PR base … head | honours `selected_by`; strict with the `e2e-real-machine` label |
| `merge_group` | queue base … queue head | strict |
| push to `master` | `before` … head (a new branch selects everything) | honours `selected_by` |
| schedule, manual, `all: true` | none | everything |

Changed files map to artifacts through the `paths` of each recipe in
`release/targets.json`. For Go artifacts (control-plane, inference-gateway,
Forge) the mapping is refined with `go list -deps -json`: a non-test `.go` file
counts only when its package is compiled into the binary, and a non-Go file
counts only when it is embedded or listed as a build input. Artifacts then map to
scenarios through each scenario's compiled `required_artifacts`. A change under
`<owner>/test/e2e/` selects every runnable scenario of that owner.

The rules, applied in order:

1. **Documentation only** (`*.md`, `docs/**`, `**/docs/**`, `LICENSE`) selects
   nothing. Both aggregates pass with only the `select` job.
2. **CI change** (`.github/**`, `.githooks/**`, `testkit/**`, `release/**`, the
   root `Makefile`, `go.work`, lint configuration, `.gitignore`,
   `.gitattributes`) selects every CI job plus each suite's smoke scenario:
   `charts/fresh-install`, `control-plane/deployed-control-plane`, and
   `forge/cpu`.
3. **Version-only change** (a component `VERSION` file, or a `Chart.yaml` diff
   that only moves `version`, `appVersion`, or dependency versions) selects
   `charts-static`, `ci-contract`, and the install-readiness smoke
   `charts/fresh-install`.
4. **Chart change** templates each scenario's declared `renders` at base and
   head and selects the scenarios whose rendered manifests differ. A render that
   fails on either side counts as changed. A scenario that declares no renders
   is selected by any change to a chart it requires.
5. **GPU driver inputs** (`forge/internal/gpu/**`,
   `forge/internal/config/gpu*.go`, `forge/test/e2e/gpu_upgrade_test.go`, or a
   `remote-content.json` diff that touches the NVIDIA driver or GPU Operator)
   select the optional `driver-upgrade` stage. Otherwise that stage is recorded
   `not-selected`.
6. **A path with no owner** selects everything: every CI job, every runnable
   scenario, and the `driver-upgrade` stage.

Rules 2–5 add to the ordinary owner selection of any other changed path in the
same diff.

### Pull-request narrowing and strict selection (`DES-HOR-590-02`)

The Forge F3 scenarios declare `selected_by: [forge-binary]`. On a pull request
they run only when the Forge binary or `forge/test/e2e` changed, or as the CI
smoke (`forge/cpu`). A control-plane or chart change therefore gets Kind
feedback without waiting for real-machine hosts.

The merge queue runs the same selector in strict mode and ignores
`selected_by`: any change to an artifact a scenario deploys selects it, so
real-machine regressions are caught before `master`. A merge that changes
F3-deployed artifacts takes about 20–22 minutes in the queue and can bounce
there. Previews are the per-push real-machine canary and nightly full
validation is the backstop.

Two labels widen a pull request's validation:

| Label | Effect |
| --- | --- |
| `e2e-real-machine` | strict selection on the pull request itself. Agent skills add it for AgentPool, LVM, storage, and bootstrap work. |
| `full-validation` | runs `full-validation.yml` with the pull request's own workflow files. Use it for every CI change and whenever asked. |

A `remote-content.json` change is a CI change (rule 2), so on its own it runs
only the smoke scenarios, even when it moves GPU driver inputs. Add the
`full-validation` label to prove the driver upgrade before merge.

## CI jobs (`ci.yml`)

| Job | Selected by | Runs |
| --- | --- | --- |
| `ci-contract` | `.github/**`, `release/**`, `testkit/**`, version changes | `make testkit-test` and `make release-check` (selector, release plan, bump, AWS substrate, remote-content and cache contracts) |
| `control-plane` | `control-plane/**`, protobufs | format, lint, build, unit, integration and envtest |
| `dashboard` | `control-plane/ui/**` | typecheck, component tests, production build |
| `harness` | `control-plane/harness/**`, protobufs | typecheck, tests, the required Linux isolation gate |
| `tool-runner` | `control-plane/tool-runner/**`, protobufs | typecheck, tests, build |
| `protobuf` | `control-plane/proto/**`, `control-plane/buf.*` | lint, regenerate, verify committed stubs |
| `inference-gateway` | `inference-gateway/**` | format, vet, lint, build, unit and testcontainers tests |
| `forge` | `forge/**` | format, lint, GoReleaser config, build, unit and fake-SSH tests |
| `forge-fault-matrix` | with `forge` | the privileged, fail-closed data-storage fault matrix (HOR-545) |
| `charts-static` | `charts/**`, version changes | `make check` (lint, template, kubeconform, contract scripts, the appVersion link lint) and `make check-tls` |
| `nested-go-lint` | `*/test/e2e/**`, `testkit/**` | `golangci-lint run ./...` in the four nested E2E modules |

A CI change (rule 2) or a path with no owner (rule 6) selects every job.
`make lint` is the local equivalent of the lint jobs. The nested modules
`forge/test/e2e` and `control-plane/test/e2e` carry their own `.golangci.yml`
(the component linter set, a gocyclo ceiling of 25, and six harness-inapplicable
gosec rules excluded); `charts/test/e2e` and `testkit/e2e` use the default
policy.

## E2E: build once, then Kind and real machines (`e2e.yml`, C1, C3)

1. **select** runs the selector and `.github/scripts/e2e_inputs.py plan`, which
   splits the selected scenarios into a Kind (F2) matrix and a real-machine (F3)
   matrix and, when anything is selected, emits the image build matrix.
2. **build** builds each image recipe in `release/targets.json` once with
   `docker/build-push-action` and the GHA layer cache, and pushes it to
   `ghcr.io/iterabase/preview/<image>:sha-<sha>` (C9). `VERSION` comes from the
   target's version file and `COMMIT` is the source SHA (C8). An existing
   `sha-<sha>` tag is reused, so a push to `master` reuses the merge-queue build
   of the same commit.
3. **resolve images** resolves every `sha-<sha>` tag to its registry digest.
   Every consumer below pulls by that digest.
4. **kind** runs one scenario per job on a GitHub-hosted runner.
   `e2e_inputs.py prepare` pulls each required image by digest, saves it as an
   archive, builds the source charts' dependencies once, and exports the
   [scenario environment](#scenario-environment). The job then runs the owner's
   Make target.
5. **fixture images** calls `bake.yml` for the needed capacities before any
   real-machine scenario. It is a no-op when this tree's generation is already
   baked.
6. **real-machine** runs one F3 scenario per job on its own fresh EC2 host:
   `aws_ci.py launch-fixture` boots the baked AMI (and, for GPU, restores the
   model-cache snapshot), pins the host key, and exports the host environment;
   `e2e_inputs.py prepare` adds the artifacts; `make -C forge <target>` runs the
   scenario; `aws_ci.py cleanup-run` removes the host on every outcome. The host
   carries an `iterabase-ci-deadline` at the scenario timeout plus 10 minutes, so
   the reaper removes it if cleanup never runs.
7. **preview** deploys the commit's preview (see [Previews](#previews-c6-c9-c12)).
   It is not part of `E2E / required`.
8. **`E2E / required`** requires `select`, and requires `build`, `images`,
   `kind`, `bake`, and `real-machine` to succeed when they had work and to be
   skipped when they had none.

A push to `master` only builds (and updates `staging`); the merge queue already
ran the scenarios on that exact commit. Nothing from an earlier run is reused
within a pull request: each push re-runs its selected scenarios (C4).

Real-machine jobs assume the `iterabase-ci` role through GitHub OIDC. Fork pull
requests cannot obtain that token, so a change that needs real-machine scenarios
must come from a branch in this repository.

### Scenario environment

Every scenario receives `ITERABASE_E2E_FIXTURE_MODE=source`,
`ITERABASE_E2E_SOURCE_SHA`, `ITERABASE_E2E_SOURCE_DIRTY=false`,
`ITERABASE_E2E_REQUIRED=true`, and `ITERABASE_E2E_OPTIONAL_STAGES`. For each
required image prefix (`CONTROL_PLANE`, `HARNESS`, `TOOL_RUNNER`,
`INFERENCE_GATEWAY`, `FORGE_E2E_RUNTIME`) it receives
`<P>_IMAGE_{REPO,TAG,DIGEST,CONFIG_DIGEST,SOURCE_SHA,ARCHIVE}`. Chart scenarios
receive `ITERABASE_PLATFORM_LOCAL_CHART` and `ITERABASE_CHART_VERSION`.
`charts/n-1-upgrade` receives `ITERABASE_E2E_N1_*` for the newest published
platform-chart Release.

F3 scenarios additionally receive `FORGE_E2E_BINARY`,
`FORGE_E2E_{PLATFORM,SUBSTRATE,LVM_STORAGE}_CHART_ARCHIVE`, the
`FORGE_E2E_*_IMAGE_ARCHIVE` aliases, and the host contract from
`aws_ci.py launch-fixture`:

| Variable | Value |
| --- | --- |
| `FORGE_E2E_FIXTURE` | `true` |
| `FORGE_E2E_FIXTURE_ADDRESS`, `FORGE_E2E_FIXTURE_SSH_USER`, `FORGE_E2E_FIXTURE_SSH_KEY_PATH` | the fresh host and its per-run client key |
| `FORGE_E2E_FIXTURE_SSH_HOST_KEY` | the host key generated at launch and pinned (HOR-521) |
| `FORGE_E2E_FIXTURE_DATA_STORAGE_DEVICES` | the data volume's `/dev/disk/by-id/...` path |
| `FORGE_E2E_IMAGE_CACHE_ROOT`, `FORGE_E2E_IMAGE_CACHE_GENERATION` | the pinned image cache baked into the AMI |
| `FORGE_E2E_MODEL_CACHE_DEVICE`, `FORGE_E2E_MODEL_CACHE_UUID` | GPU only: the restored model-cache volume |
| `AWS_CI_FIXTURE_REGION`, `AWS_CI_FIXTURE_INSTANCE_ID` | where the host runs |

## Baked fixture images (`bake.yml`, `DES-HOR-590-03`)

Fixture AMIs hold software inputs only: Ubuntu 24.04, host packages, and the
shared pinned image cache from `remote-content.json`. They never hold cluster
state. The GPU AMI carries the same shared cache on a larger root volume; the
GPU-only NVIDIA (`nvcr.io`) and vLLM images are not baked yet and are pulled by
digest at run time. The model cache is a separate EBS snapshot of the model
pinned in `forge/test/e2e/model-cache.json`.

`aws_ci.py bake-ami` and `bake-model-cache` launch a builder from the Canonical
AMI with a per-run pinned host key, run the bake over SSH, create the image or
snapshot with CI tags, copy it to the three CI regions, and terminate the
builder. Each product is tagged with the generation it carries (a hash of the
pinned content), so a bake is a no-op when that generation exists in every
region; `force` rebakes. Because `e2e.yml` calls the bake before real-machine
scenarios, a pull request that changes pinned inputs bakes its own generation.
A missing generation fails the launch; there is no fallback to an older one.

## Reaper (`reaper.yml`)

The `reap` job terminates every instance tagged `iterabase-ci=true` whose
`iterabase-ci-deadline` has passed, or which is older than 180 minutes when it
has no deadline, in all three regions. Untagged instances are reported and never
touched. The `images` job keeps the two newest baked generations per fixture role
and of the model cache and deletes older AMIs and snapshots. Both support
`dry_run`. See [`runbooks/aws-ci.md`](runbooks/aws-ci.md#operations).

## Previews (C6, C9, C12)

| Aspect | As built |
| --- | --- |
| Environments | `pr-<N>` for a same-repository pull request whose selection builds images; `staging` for every push to `master` that builds images. GitHub Deployments carry the "View deployment" link. |
| Host | one spot `m6i.xlarge` per environment from the CPU fixture AMI, found or launched by `aws_ci.py preview-up`. Its host key is generated at creation and stored in the `iterabase-ci-host-key` tag; later runs read it through the authenticated EC2 API. Runner SSH uses the `PREVIEW_SSH_KEY` repository secret. |
| Deploy | `.github/scripts/preview.py` builds the commit's Forge, imports the build-once images into the host's containerd (verified by config digest), re-versions the platform and substrate charts as `<version>-pr.<N>.<run>` or `<version>-main.<run>` and pushes them to `ghcr.io/iterabase/preview/charts`, and runs `forge apply` from host-local copies of those charts. The first deploy bootstraps k3s and data storage first; later pushes upgrade in place. |
| Values | the versioned fixture overlay `forge/test/e2e/overlay` with the preview values appended, committed on the host as a `file://` repository. Forge serves it to Flux over read-only node SSH (DES-HOR-632-01), so no overlay token or external repository is involved. The F3 scenarios use the same fixture. |
| Inference | an external `ModelBackend` to the capped hosted API (`PREVIEW_LLM_BASE_URL`, `PREVIEW_LLM_MODEL`, `PREVIEW_LLM_API_KEY`). No GPU. A `qa-operator` IdentityMapping is seeded. |
| Access | Tailscale only. The workflow mints a one-hour, single-use, ephemeral `tag:preview` key through the Tailscale OAuth client (`TAILSCALE_OAUTH_CLIENT_ID`, `TAILSCALE_OAUTH_SECRET`); the host joins as `iterabase-<environment>` and `tailscale serve` exposes the control-plane API (Dashboard at `/`, API at `/v1`) at `https://<host>.<tailnet>.ts.net` and the OpenAI-compatible inference gateway at `https://<host>.<tailnet>.ts.net:8443/v1`, straight to their Services so no ingress host name has to match. Nothing is public. QA agents and runners join with their own ephemeral keys. |
| Concurrency | one group per environment; the latest push wins. |
| Teardown | `preview.yml` terminates the host and marks its deployments inactive when the pull request closes or merges. Each deploy renews a 72-hour `iterabase-ci-deadline`, so the reaper removes a preview idle for 72 hours. `staging` is never TTL-expired. |

Previews are evidence, not a gate. Full validation does not deploy them.

## Full validation (`full-validation.yml`, C11)

Full validation calls `ci.yml` and `e2e.yml` with `all: true` for one exact
commit: every CI job, every Kind and real-machine scenario, the GPU
`driver-upgrade` stage, and `charts/n-1-upgrade`. It publishes nothing.

- **Nightly** at 02:17 UTC, skipped when the last successful scheduled run
  already validated the current `master` SHA.
- **On a pull request** through the `full-validation` label. It uses the pull
  request's own workflow files, so it also works for a CI change before the
  workflow exists on `master`.
- **Manual** dispatch, and **from `release.yml`** with the release SHA and
  target set, where images of targets not being released come from their
  published versions (C7).

The `report` job writes queue wait and duration for every job to the run
summary and, outside pull requests, opens or comments on the single
`Full validation is red` issue when it fails.

## Merge queue (C4)

`master` is protected by a merge queue that requires `CI / required` and
`E2E / required`. The queue runs the selector in strict mode on the exact merge
commit. Merge queue is available because the repository is public and owned by
the `iterabase` organization (C13). If the repository becomes private, the queue
needs GitHub Enterprise Cloud; the fallback is strict up-to-date protection.

## Caches (C3)

| Cache | Key |
| --- | --- |
| Go modules and build cache | Go version, job scope, and the digest of the supplied `go.sum` files; restores roll to the newest entry for the same version and scope |
| npm downloads | Node version, job scope, and the lockfile digest |
| Helm downloads | Helm version, job scope, and the lock digest; restores roll to the newest entry |
| CI tool archives (`setup-ci-tool`) | tool name, version, and the reviewed `remote-content.json` SHA-256 |
| Kubernetes tools | the pinned tool set |
| Playwright browsers | Playwright version and the reviewed archive SHA-256 |
| Docker layers | GHA cache per image recipe (`image-<recipe>`, `mode=max`) |
| Built images | the `sha-<sha>` tag in the preview registry |

Every cached input is verified against `.github/inputs/remote-content.json`
before use. Clusters, databases, credentials, runtime bundles, and results are
never cached. There are no automatic scenario retries or accepted flakes.

## Budgets

Targets from the approved plan, with the measure-first results (2026-10-08):

| Change shape | Before (excluding queue) | Target |
| --- | --- | --- |
| Docs | 0 | 0 |
| CI-only or version bump | 25–48 min | ≤ 8 min |
| Control-plane or chart | 45–50 min | 8–12 min |
| Forge or GPU | 46 min + queue | ≈ 20–22 min |
| Merge queue | n/a | same as the pull-request shape |
| Preview first create / update | n/a | ≈ 12 min / ≈ 3 min after CI |
| Official release | 52–189 min | full validation (≈ 40 min) + approval + ≈ 5 min promotion |

| Measurement | Result |
| --- | --- |
| Preview host SSH reachable after launch | ≈ 30 s |
| Preview `forge apply` create (k3s, certificate and LVM substrates, Flux, platform) | 165 s |
| Preview `forge apply` upgrade / no-op reapply | 53 s / 52 s |
| Merged control-plane Kind install prefix (Kind 43, images 4, certificate 20, LVM 57, control plane 35) | ≈ 160 s |
| Identity / work / artifact / browser journeys on that install | 12 / 12 / 44 / 14 s (≈ 5 min per scenario) |

Full-validation run summaries record per-job durations and queue wait (HOR-614).

## Adding a scenario

Register it in the owner's `TestE2E` (`charts/test/e2e`,
`control-plane/test/e2e`, or `forge/test/e2e`) with `testkit/e2e`
`ScenarioMetadata`:

| Field | Meaning |
| --- | --- |
| `name`, `description`, `references` | identity and the tickets or decisions it proves |
| `tier` | `F2` (Kind) or `F3` (real machine); `F0` examples are not run by CI |
| `required_artifacts` | recipes from `release/targets.json` the scenario deploys; drives selection and the inputs it receives |
| `fixture_modes` | `source` |
| `make_target`, `timeout_minutes` | the owner Make target CI runs and its bound |
| `capacity`, `mandatory_capacity` | F3 only: `cpu` or `gpu` |
| `smoke` | the one scenario per suite that CI-only changes run |
| `selected_by` | optional narrower artifact set that selects it on pull requests; the merge queue ignores it |
| `renders` | the chart, values files, and `--set-string` values it installs, so a chart change selects it only when its rendered manifests change |

A stage marked `Optional` runs only when `ITERABASE_E2E_OPTIONAL_STAGES` names
it, and no stage may depend on it. An optional stage also needs its selection
rule in `affected.py`. Run `make testkit-test` and `make release-check`; the
compiled catalogue is the only scenario list.

## Local contract validation

```bash
make testkit-test     # shared mechanics, owner examples, compiled catalogue
make release-check    # selector, release plan, bump, AWS substrate, remote content, caches
make -C charts check  # includes the appVersion link lint
go run ./testkit/e2e/cmd/e2e-catalogue --format json --output /tmp/catalogue.json
python3 .github/scripts/affected.py --catalogue /tmp/catalogue.json --base origin/master
```
