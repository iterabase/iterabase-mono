# Shared E2E execution contract

`testkit/e2e` owns the typed suite/stage runner, compiled catalogue, and explicit
source fixture record. Product, chart, browser, and Forge assertions remain in
their owner modules.

## Compiled metadata

Every runnable F2/F3 scenario declares its required artifacts, the `source`
fixture mode, owner Make target, timeout, mandatory capacity where applicable,
and its stage DAG. `make e2e-catalogue` compiles the real owner `TestE2E`
registrations. A missing artifact, target, fixture mode, timeout, or stage fails
catalogue validation.

## Required execution

CI runs one scenario per job and judges it by the job result. It sets:

- `ITERABASE_E2E_FIXTURE_MODE=source`, `ITERABASE_E2E_SOURCE_SHA` (full 40-hex)
  and `ITERABASE_E2E_SOURCE_DIRTY=false`
- `ITERABASE_E2E_REQUIRED=true`
- `ITERABASE_E2E_OPTIONAL_STAGES`, the comma-separated optional stages the
  affected selector chose (may be empty)

Owner suites additionally receive the exact source-built artifacts: for each
required image prefix `<P>_IMAGE_{REPO,TAG,DIGEST,CONFIG_DIGEST,SOURCE_SHA}` and
a `docker save` archive of `repo:tag`, plus the dependency-built platform chart
directory. F2 scenarios with image requirements declare `create-kind` followed
by `import-runtime-images`; every later stage depends transitively on that
import. The shared Kind helper restores the archive and transports the exact
reference into the created nodes, so pre-cluster runner-daemon state cannot
silently satisfy an install. It verifies the archive's config digest and returns
the imported single-platform manifest digest. Owners then prove that workloads
requested the supplied reference, that its revision label matches the source
SHA, and bind it to the immutable identity exposed by that CRI. Kind Pod status
reports the imported manifest digest; K3s Pod status reports the
already-verified config digest, so Forge also proves the imported
tag-to-manifest mapping separately. Registry, config, and runtime-manifest
digests remain distinct identities rather than being compared as if they were
interchangeable.

Kind scenarios that create an `AgentPool` use the shared storage helper before
platform data claims or workers. It removes Kind's local-path/default fallback,
prepares a real loop-backed thick `iterabase-data` VG in the one privileged node,
installs the exact source `lvm-storage-substrate` with the real
`/var/lib/kubelet` registration path used by both Kind and supported K3s, and
waits for exact OpenEBS CRD/controller/node/CSINode-topology/VG readiness. It
verifies exactly the non-default,
grow-only-expandable, thick XFS/RWO `iterabase-lvm-xfs` and
`iterabase-agentpool-lvm-xfs` classes, including the latter's same-node
`shared: yes` boundary.

The runner records exactly one terminal status for every declared stage. Failed
or skipped prerequisites block only dependents, so independent work,
diagnostics, and cleanup continue. With `ITERABASE_E2E_REQUIRED=true` any direct
skip or blocked/not-run stage fails the scenario, so mandatory CPU/GPU capacity
cannot pass by skipping. An optional stage that `ITERABASE_E2E_OPTIONAL_STAGES`
does not name is recorded `not-selected`, which is complete rather than skipped.

## Validation

```bash
make testkit-test
make e2e-catalogue
```
