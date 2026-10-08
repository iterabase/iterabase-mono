# Forge E2E runner design

Original composition decision: HOR-406, approved 2026-08-03. Permanent-fixture
and exact-artifact cutover: `DES-HOR-540-01` and `DES-HOR-540-02`, approved
2026-09-01.

## Goals

- One compiled `TestE2E` entrypoint and one stage DAG per scenario, run in the
  `source` fixture mode.
- Exact CI-supplied source-built images, chart archives, Forge binary, and
  runtime fixture; no owner-local build/load fallback in required execution.
- Real CPU/GPU substrate behavior without provider availability or provider API
  credentials in Actions.
- A proven clean destroy/apply/test/destroy boundary on dedicated, reimageable
  fixtures.
- Fail-closed fixture, artifact, and stage identities with no retry or
  selected-capacity skip.

## Permanent fixture authority

F3 uses exactly one founder-provisioned CPU host and one founder-provisioned GPU
host. Repository variables supply each fixed address, SSH user, pinned OpenSSH
host public key, and Forge data-storage `/dev/disk/by-id/...` identity. A separate
repository secret supplies each fixture-scoped private key. These values are not
workflow-dispatch inputs.

Every fixture path uses its literal `iterabase-permanent-fixture-<capacity>`
concurrency group with `cancel-in-progress: false` and FIFO `queue: max` across
PR, master, and candidate execution. Work targeting the same host is serial; the
independent CPU and GPU hosts may overlap. Build/unit/F2 work remains parallel.

Actions has no provider credential. It cannot list, create, delete, resize,
power-cycle, rescue, reimage, or replace a fixture. If strict SSH cleanup and
reboot cannot recover a host, F3 stops until founder-operated provider recovery
restores the runbook baseline.

## Lifecycle boundary

Before every selected scenario, and unconditionally after diagnostics on every
outcome, the harness runs:

```text
forge destroy --purge-data-storage --reboot --yes
```

Ordinary `forge destroy` is unchanged and preserves AgentPool workspace state.
The explicit purge runs only after the existing platform/K3s destroy path. It
revalidates the configured stable whole disk, root/system exclusion, holders and
active consumers, hardware identity, receipt, PV/VG UUIDs, ownership tag, and
exact membership. Missing, ambiguous, wrong, in-use, or drifted state refuses.
A clean second purge is idempotent only when the configured disk is blank and
every Forge receipt/PV/VG authority surface is absent. Reboot is last.

The harness requires strict host-key verification, observes SSH disconnect,
requires reconnect with a changed `/proc/sys/kernel/random/boot_id`, and proves
that K3s, data-storage receipt/PV/VG/signature state, run-scoped
overlay/transferred state, and stale test processes cannot satisfy the next
scenario. Failure is
incomplete/failing, never a skip.

## GPU model cache

The GPU fixture has a harness-owned block volume mounted at `/data/hf-cache`.
Its fixed by-id device and filesystem UUID must differ from the Forge AgentPool
data-storage device. Forge does not configure, authorize, purge, or claim this
volume.

[`model-cache.json`](model-cache.json) pins the public
`Qwen/Qwen3.5-0.8B` model at revision
`2fc06364715b967f1860aea9cf38778875588b17` and pins the selected weight file to
SHA-256 `04b1c301231dd422b8860db31311ab2721511346a32cb1e079c4c4e5f1fe4696`.
Every GPU use verifies device, mount, UUID, revision path, and content hash
before product execution. The harness copies that exact cache into a
controller-owned general-class ModelBackend PVC before serving. The serving pod
mounts only the PVC at `/data/hf-cache`; the fixture disk is read-only seed input,
not product persistence. The scenario then proves mounted HF and generic-cache
growth plus pod-replacement byte identity. Cache bytes cannot satisfy product
artifact/runtime identity or AgentPool workspace assertions.

## Compiled scenarios

### `permanent-fixture-cpu`

Uses the CPU fixture for no-GPU refusal, supported migration-source install,
exact current Forge/chart/image/Flux handoff, receipt-bound OpenEBS LVM assertions,
two-worker RWO behavior, persistence/replacement/reapply, secret sync, and Flux
reconciliation.

### `permanent-fixture-cpu-workspace`

Resets the same CPU fixture, then proves process-open raw-device refusal, exact
workspace identity, concurrent isolated work, capacity gating, human-gate
worker replacement, persisted bytes, and idempotent reapply.

### `permanent-fixture-gpu`

Uses the GPU fixture for exact baseline/candidate driver transition, real GPU
smoke, disposable `emptyDir` versus durable cache behavior, exact platform
handoff, pinned model-cache validation, and one non-authoritative real-serving
request. Ordinary unit and F3 assertions enforce the supported
`deleteEmptyDir=true` policy without a ticket-specific negative workflow.

Forge registers no chart-install-only Kind scenario. Product and chart behavior
remains in control-plane/charts owner suites; Forge's deployed checks are
bounded dependent smokes after Forge-owned substrate and handoff assertions.

## Exact artifact contract

CI builds each affected image once from the exact source SHA, pushes it to the
preview registry, and exports its repository, tag, registry digest, config
digest, source SHA, and `docker save` archive, together with `FORGE_E2E_BINARY`
and the `FORGE_E2E_{PLATFORM,SUBSTRATE,LVM_STORAGE}_CHART_ARCHIVE` packages of
the source charts.

The Forge stages transfer only those supplied bytes and separately verify
requested references, archive config/source labels, imported K3s CRI config
identity, and remote tag-to-manifest identity. A source-only build/load path in
required execution, stale host byte, or missing artifact is a failure.

With `ITERABASE_E2E_REQUIRED=true` the scenario's Go test result is its
verdict: any skipped, blocked, or not-run mandatory stage fails it, so selected
CPU/GPU capacity cannot pass by skipping. The GPU fixture additionally verifies
the model-cache device, mount, UUID, and pinned model revision hash before use.

## Qualification and legacy removal

Ephemeral provider-managed host provisioning and tagged reaping were retained
only on the HOR-540 branch while the permanent path was qualified. Removal was authorized
only after the dated lifecycle acceptance record contained three consecutive
CPU and three consecutive GPU green destroy/apply/test/destroy cycles; any
failed or incomplete cycle reset that fixture's streak. The final repository has
no provider SDK, dynamic capacity discovery, `FORGE_E2E_KEEP`, tagged reaper, or
provider API-token workflow path.

## Operational authority

Setup, SSH pinning, model-cache preparation, key rotation, quarantine, manual
provider recovery, and rollback are in
[`../../../docs/runbooks/permanent-e2e-fixtures.md`](../../../docs/runbooks/permanent-e2e-fixtures.md).
Qualification run IDs and bound identities are retained in the dated HOR-540
lifecycle acceptance record.

The production impact is the explicit Forge purge/reboot CLI and permanent
fixture operations. Semantic publication is not required for HOR-540
acceptance; its all-target candidate is validation-only and must not be
promoted.
