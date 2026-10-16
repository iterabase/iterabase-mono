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

Three fields drive selection by `.github/scripts/affected.py` (C2):

- `smoke` marks the one scenario per suite that CI-only changes run;
- `selected_by` optionally narrows which changed artifacts select the scenario
  on pull requests; the merge queue and the `e2e-real-machine` label ignore it
  (`DES-HOR-590-02`);
- `renders` lists the chart, values files, and `--set-string` values the
  scenario installs, so a chart change selects it only when its rendered
  manifests change.

A stage marked `Optional` runs only when `ITERABASE_E2E_OPTIONAL_STAGES` names
it, and no stage may depend on it. See [`../../docs/ci.md`](../../docs/ci.md#adding-a-scenario).

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

## Opt-in Kind failure evidence (DES-HOR-682-01)

Chart diagnostics set `diagnostics.Collector.KindClusterName` to collect runtime
observations **before** Kubernetes/Helm evidence and teardown. Existing callers
that leave it empty keep their capture defaults. The existing diagnostics upload
contains `kind-runtime/index.json` and separate stdout/stderr logs for node
identity, the last 500 containerd/kubelet journal entries, and `ctr --namespace
k8s.io content active`. No pull, content-body read, configuration read/change,
cache repair, debug enablement, or retry is performed. The known CRI startup
configuration-dump record is withheld from journal evidence too.

Runtime collection has a two-minute overall deadline, 20 seconds per command,
1 MiB combined stdout/stderr per command and 8 MiB for all new runtime files,
including the index. At most 16 nodes are sampled to bound metadata cardinality;
extra nodes are explicitly reported omitted. Output overflow drains but withholds
**both complete payloads**, rather than exposing a truncated secret. The
opt-in process capture policy sanitizes URLs before structural redaction and any
process-file write: only endpoint origins and public digest-shaped path identities
survive. URL quotes/apostrophes are consumed with the potentially sensitive token,
not trusted as delimiters that could expose a signed suffix. Captures/errors with
credential-header keys (Authorization, Proxy-Authorization, Cookie or Set-Cookie)
are withheld **in full** with an explicit privacy marker, rather than partially
redacting untrusted scalar/array/multiline records. Header classification inspects
up to eight escape layers in a separate copy, covering quoted JSON/logfmt and
Unicode keys; ambiguous deeper encodings also fail closed. This deliberately may
lose unrelated context from the same credential-bearing capture. Ordinary
`Redactor.String` callers are unchanged. Failed, empty, unavailable, truncated and budget-exhausted sources
remain explicit in the private index; a populated partial file is not a successful
observation. A nonempty runtime output directory is never overwritten/recollected.

The containerd 1.7.18 `active` CLI's `SIZE` column is a **humanized current ingest
offset**, not an exact expected total or the byte count of the historical failed
HTTP request. An empty table means only that no ingest was visible at sampling.
Default journals may not name a failed descriptor or endpoint. These artifacts
close a retention gap; they do not prove a download fixed, change the original
scenario result, or suppress independent cleanup. Node/protocol parsing uses
stdout, never stderr/combined diagnostics.

`make testkit-kind-example` exercises these actual read-only commands on its
existing disposable lifecycle fixture before independent teardown, without an
Alertmanager pull or registry-failure experiment.

## Validation

```bash
make testkit-test
make e2e-catalogue
```
