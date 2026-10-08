# Forge E2E runner design

Original composition decision: HOR-406, approved 2026-08-03. Exact-artifact
execution: `DES-HOR-540-02`, approved 2026-09-01. Fresh per-run AWS hosts and
test placement: C1 and C5 of the CI/CD overhaul and `DES-HOR-590-03`, approved
2026-10-08, on the HOR-591 substrate (`DES-HOR-591-01/02`).

## Goals

- One compiled `TestE2E` entrypoint and one stage DAG per scenario, run in the
  `source` fixture mode.
- Exact CI-supplied source-built images, chart archives, Forge binary, and
  runtime fixture; no owner-local build/load fallback in required execution.
- Real CPU/GPU substrate behavior on a host that no earlier run has touched.
- Fail-closed host, artifact, and stage identities with no retry or
  selected-capacity skip.

## Fresh per-run hosts (C1)

Each selected F3 scenario gets its own EC2 instance in the `iterabase-ci`
account. Scenarios run in parallel, and a superseded pull-request run cancels.

`aws_ci.py launch-fixture` boots the instance from the fixture AMI baked for this
tree's pinned image cache, with a per-run SSH client key and a host key that is
generated at launch and pinned before the first connection (HOR-521). It
attaches a blank data volume and, on GPU, a volume restored from the model-cache
snapshot. GPU hosts walk the approved GPU types and the three CI regions
(`DES-HOR-591-02`). The instance carries the CI tags and an
`iterabase-ci-deadline`; `aws_ci.py cleanup-run` terminates it after the
scenario on every outcome, and the reaper terminates it if cleanup never runs.

The launch exports the host contract:

| Variable | Value |
| --- | --- |
| `FORGE_E2E_FIXTURE` | `true` |
| `FORGE_E2E_FIXTURE_ADDRESS`, `FORGE_E2E_FIXTURE_SSH_USER`, `FORGE_E2E_FIXTURE_SSH_KEY_PATH` | the host and its per-run client key |
| `FORGE_E2E_FIXTURE_SSH_HOST_KEY` | the pinned host public key |
| `FORGE_E2E_FIXTURE_DATA_STORAGE_DEVICES` | the data volume's `/dev/disk/by-id/...` path |
| `FORGE_E2E_IMAGE_CACHE_ROOT`, `FORGE_E2E_IMAGE_CACHE_GENERATION` | the pinned image cache in the AMI |
| `FORGE_E2E_MODEL_CACHE_DEVICE`, `FORGE_E2E_MODEL_CACHE_UUID` | GPU only: the model-cache volume |
| `AWS_CI_FIXTURE_REGION`, `AWS_CI_FIXTURE_INSTANCE_ID` | where the host runs |

A missing or empty variable, a host-key mismatch, a missing baked generation, or
an unreachable host fails the scenario. It never becomes a skip.

## Baked inputs (`DES-HOR-590-03`)

Fixture AMIs hold software inputs only: Ubuntu 24.04, host packages, and the
shared pinned image cache from `.github/inputs/remote-content.json`. They never
hold cluster state. The GPU AMI carries the same shared cache on a larger root
volume; the GPU-only NVIDIA (`nvcr.io`) and vLLM images are not baked yet and are
pulled by digest at run time. Real-machine jobs import and verify the cached
archives before any apply, so the platform applies do not pull those images from
public registries.

`bake.yml` produces the AMIs and the model-cache snapshot with
`aws_ci.py bake-ami` and `bake-model-cache`, keyed by the content they carry. CI
calls it before real-machine scenarios, so the bake is a no-op unless this tree
changed the pinned inputs. See [`../../../docs/ci.md`](../../../docs/ci.md#baked-fixture-images-bakeyml-des-hor-590-03).

## Lifecycle boundary

The first stage of every scenario, `prepare-fresh-host`, waits for SSH with
strict host-key verification, waits for the data volume's by-id path, and proves
that nothing is installed: no k3s, no data-storage receipt, no `iterabase-data`
VG or PV, and no filesystem signature on the data device. On GPU it also
verifies the model cache. There is no pre-run destroy, purge, or reboot because
the host is new.

Diagnostics run after a failure and before the host is terminated. The
explicit `forge destroy --purge-data-storage --reboot --yes` decommission path
remains product behavior and keeps its unit and fault-matrix coverage; F3 does
not need it for isolation.

## GPU model cache

The GPU host has a harness-owned volume restored from the model-cache snapshot
and mounted read-only at `/data/hf-cache` by filesystem UUID. Its by-id device
and UUID must differ from the Forge data-storage device. Forge does not
configure, authorize, purge, or claim this volume.

[`model-cache.json`](model-cache.json) pins the public `Qwen/Qwen3.5-0.8B` model
at revision `2fc06364715b967f1860aea9cf38778875588b17` and pins the selected
weight file to SHA-256
`04b1c301231dd422b8860db31311ab2721511346a32cb1e079c4c4e5f1fe4696`. Every GPU use
verifies device, mount, UUID, revision path, and content hash before product
execution. The harness copies that exact cache into a controller-owned
general-class ModelBackend PVC before serving. The serving pod mounts only the
PVC at `/data/hf-cache`; the host volume is read-only seed input, not product
persistence. Cache bytes cannot satisfy product artifact/runtime identity or
AgentPool workspace assertions.

## Compiled scenarios (C5)

| Scenario | Capacity | Make target | Proves |
| --- | --- | --- | --- |
| `cpu` (CI smoke) | cpu | `test-e2e` | GPU refusal on a CPU host, the exact Flux handoff, durable inotify capacity, idempotent reapply, secret sync, and Flux reconciliation |
| `cpu-workspace` | cpu | `test-e2e-workspace` | process-open raw-device refusal, PV/VG identity and classes, two-worker RWO AgentPool, concurrent isolated work, capacity gating, human-gate worker replacement, XFS/LVM growth, reboot and reapply with persisted bytes, claim release, and non-purging ordinary destroy |
| `gpu` | gpu | `test-e2e-gpu` | GPU substrate readiness, GPU smoke, exact platform handoff, model-cache seeding, and one real serving completion; the optional `driver-upgrade` stage proves an `emptyDir`-safe driver transition |

`cpu` and `cpu-workspace` run on separate parallel hosts; LVM and AgentPool
behavior belongs to `cpu-workspace` only. The `driver-upgrade` stage is
`Optional`: it runs when `ITERABASE_E2E_OPTIONAL_STAGES` names it (GPU driver
inputs changed, or full validation) and is recorded `not-selected` otherwise.

All three declare `selected_by: [forge-binary]`, so on a pull request they run
when the Forge binary or this module changes (and `cpu` as the CI smoke); the
merge queue and the `e2e-real-machine` label select them whenever any artifact
they deploy changes (`DES-HOR-590-02`).

Forge registers no chart-install-only Kind scenario. Product and chart behavior
remains in the control-plane and charts owner suites; Forge's deployed checks are
bounded dependent smokes after Forge-owned substrate and handoff assertions.

## Exact artifact contract

CI builds each image once from the exact source SHA, pushes it to the preview
registry, and exports its repository, tag, registry digest, config digest,
source SHA, and `docker save` archive, together with `FORGE_E2E_BINARY` and the
`FORGE_E2E_{PLATFORM,SUBSTRATE,LVM_STORAGE}_CHART_ARCHIVE` packages of the source
charts.

The Forge stages transfer only those supplied bytes and separately verify
requested references, archive config/source labels, imported K3s CRI config
identity, and remote tag-to-manifest identity. A source-only build/load path in
required execution, a stale host byte, or a missing artifact is a failure.

With `ITERABASE_E2E_REQUIRED=true` the scenario's Go test result is its
verdict: any skipped, blocked, or not-run mandatory stage fails it, so selected
CPU/GPU capacity cannot pass by skipping.

## Operational authority

The AWS account, CI role, tags, regions, budget, and reaper are in
[`../../../docs/runbooks/aws-ci.md`](../../../docs/runbooks/aws-ci.md). The CI
flow is in [`../../../docs/ci.md`](../../../docs/ci.md).
