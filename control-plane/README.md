# control-plane

> Canonical source: [`iterabase-mono/control-plane`](https://github.com/nunocgoncalves/iterabase-mono/tree/master/control-plane). The former standalone repository is historical and read-only.

The Horizonshift/Iterabase control plane: a Go Kubernetes operator + HTTP API
that provides V2 customer authority (browser sessions, People, API credentials), sandbox reconciliation,
a durable turn runtime, and the model catalog the inference-gateway consumes.

Per-customer, fully isolated, self-hosted. See the Platform Direction doc
(Obsidian: *Horizonshift Platform Direction*) for the full architecture; this
monorepo directory is the source of truth for control-plane infrastructure intent.

## Status

**Walking skeleton.** HOR-241 landed the two-binary foundation (operator +
API, DB schema, config, CI). HOR-453/454/582 replace the original identity and
permission CRDs with the V2-only authority: always-on browser sign-in, People
administration, V2 API credentials, and one reviewed authority class per HTTP
route (see [Authority](#authority-hor-582)). HOR-306/268 add the model catalog (`ModelBackend`/`Model` CRDs
+ the `effective_catalog` view). HOR-246 adds the durable turn runtime: the
`runtime` schema + store (workflow_run/step/turn state machines + append-only
event/audit log) — the data layer HOR-249 (orchestration) and HOR-252 (workflow
definitions) consume. HOR-245 adds the AgentPool CRD/operator: isolated
warm-worker pods (SPIFFE certs via the cert-manager CSI driver, dedicated-class
same-node RWO sandbox PVC, deny-by-default NetworkPolicy, deny-by-default workspace-tool
switch, maximum gateway grants + credential-slot bindings) and removes the
superseded per-sandbox egress proxy (ARCH-009). Sandbox reconciliation (HOR-245)
lands in its own ticket. HOR-249 adds the durable-dispatch Work server
(`cmd/dispatch`): the warm-worker mTLS bidi stream, worker fencing, one-credit
dispatch, durable TurnEvent ACK/dedup, cancellation/worker-loss, the
active-assignment context (`runtime.turn_assignments`) that binds a running turn
to the verified worker + fencing generation, and the dispatch reconciler.
HOR-254 evolves that foundation into a single-active-node graph runtime and adds
the durable `work` domain: stable work items, first-class attempts, append-only
node visits/transitions, blockers, feedback, artifact references, business
SSE events, and an estimated-value ledger. HOR-399 adds the immutable artifact
service: Postgres lifecycle metadata, MinIO bytes, REST + workload streaming,
active-execution authorization, retention/deletion, and supervisor-controlled
materialization/publication.

## Binaries

Two Go images + one Node image:

| Binary | Path | Image | Role |
|---|---|---|---|
| `manager` | `cmd/manager` | `control-plane` | controller-runtime operator: reconcilers, webhooks, probes, metrics |
| `api` | `cmd/api` | `control-plane` | HTTP API (chi) + durable runtime (later); subcommands `serve`, `migrate up`, `migrate down`, `bootstrap` |
| `gateway` | `cmd/gateway` | `control-plane` | mTLS workload boundary: tool `RunnerService`/`GatewayService` plus streaming `ArtifactService` |
| `dispatch` | `cmd/dispatch` | `control-plane` | durable-dispatch Work gRPC server (HOR-249): mTLS `Harness.Work` bidi stream, worker fencing, one-credit dispatch, TurnEvent ACK/dedup, cancellation/worker-loss |
| `artifact-coordinator` | `cmd/artifact-coordinator` | `control-plane` | V2 artifact lifecycle worker (HOR-461): the only holder of the lifecycle object credential; leases security attempts, scans over mTLS, expands email containers, promotes cleared originals, purges and expires bytes |
| `artifact-scanner` | `cmd/artifact-scanner` | `control-plane` | isolated scanner adapter beside a pod-local `clamd` (HOR-461): no data credential, no egress; malware verdict plus the admission policy (type detection, structural validation, email expansion) |

The `control-plane` image (all six binaries) runs in the platform
namespace. The harness image is Node (`harness/Dockerfile`). See `Dockerfile`
and `harness/Dockerfile` (harness).

`migrate` runs as an RBAC-less init container before `serve`/`manager` start.

## Layout

```
cmd/manager/        K8s operator (controller-runtime)
cmd/api/            HTTP API + migrate/bootstrap subcommands
api/v1alpha1/       CRD types (AgentPool, ModelBackend, Model, Workflow); group platform.iterabase.com
internal/config/    YAML + env config (api) + DatabaseFromEnv (manager)
internal/database/  pgx pool + embedded golang-migrate migrations
internal/identity/  identities, local accounts, browser sessions, auth email, V2 API credentials, bootstrap
internal/catalog/   model catalog store (backends + models) + effective_catalog view (HOR-306/268)
internal/artifact/  V2 quarantine domain (HOR-461): zone-split MinIO, independent storage/security state,
                    shared library search, coordinator, scanner adapter, admission policy, deletion tombstones
internal/gateway/   tool gateway: registry, authorization, credential resolution, durable invocation ledger (HOR-392)
internal/dispatch/  durable dispatch Work server: warm-worker bidi stream, one-credit dispatch, worker fencing, active-assignment context, TurnEvent ACK/dedup, cancellation/worker-loss (HOR-249)
internal/spiffe/    shared SPIFFE/mTLS identity verifier (HOR-392; reused by HOR-249)
internal/controller/ CRD reconcilers (Git -> DB bridge): agentpool, modelbackend, model, workflow
internal/runtime/   durable turn + graph runtime state and technical event log — HOR-246/254
internal/work/      work items, graph orchestration, blockers, feedback, value, business events — HOR-254
internal/server/    chi HTTP routes, each registered with one authority class (auth, People, work REST + SSE, artifacts)
ui/                 React/Vite customer Dashboard embedded in the API — HOR-396
internal/logging/   shared slog logger + logr bridge
internal/version/   build-time version metadata
internal/testutil/  shared Postgres test helper (testcontainers)
config/             kubebuilder Kustomize — DEV/envtest only (prod = forge Helm)
proto/              harness RPC contract (buf) — HOR-351
harness/            Node pi harness (the agent) — HOR-351; see harness/README.md
internal/harnessrpc/ generated Go Connect stubs (HOR-249 consumes) — HOR-351
Dockerfile          Dashboard + manager/api/gateway/dispatch image (one image, four entrypoints)
```

## Develop

```bash
make ui-deps            # install locked Dashboard dependencies
make ui-test            # component/interaction/accessibility/localization tests
make build              # build Dashboard + all Go binaries
make run-manager        # run the operator locally
make run-gateway        # run the tool gateway (serve); needs DATABASE_URL + mTLS certs
make migrate-up         # apply DB migrations
make setup-envtest      # download kube-apiserver assets (for make test)
make test-unit          # fast unit tests (skips Docker/envtest)
make test               # fresh unit + envtest + integration (needs Docker; no cached results)
make harness-isolation-test # required Linux setpriv/UID/filesystem gate (needs Docker)
make lint               # golangci-lint
make fmt-check
make install-hooks      # use .githooks/pre-commit
```

The API reads `control-plane.example.yaml` (copy to `control-plane.yaml`) or
env vars (`DATABASE_URL`, `API_ADDR`, `METRICS_ADDR`, `LOG_LEVEL`, `LOG_FORMAT`,
`AUTH_*`, and `ARTIFACT_*`). The
manager reads only `DATABASE_URL` from the environment. When configured,
API/gateway/dispatch serve bounded Prometheus telemetry on a dedicated
plaintext in-cluster `METRICS_ADDR`; application ingress and mandatory-mTLS RPC
listeners never serve metrics. Manager uses controller-runtime metrics,
AgentPool supervisors serve supervisor-derived metrics on their probe port, and
disposable pi children expose no listener.

## Database

Postgres is the system of record. The initial migration (`000001_init_schemas`)
creates four schema namespaces — `identity`, `permissions`, `usage`, `ai_data` —
plus the `pgvector` extension. `000002_identity` (HOR-242) adds the identity
store; `000030_v2_only_authority` (HOR-582) moves an existing installation to
V2-only authority in one forward step.
permissions → HOR-243, durable turns/events → HOR-246; `usage`/`ai_data`
content is post-v1. The `egress` schema (HOR-244) is dropped by 000012 under
ARCH-009 (HOR-245). Requires a pgvector-enabled Postgres
image (tests use `pgvector/pgvector:pg16`).

## Authority (HOR-582)

Customer authority is V2-only and always on
([`docs/architecture/v2-authentication-authority.md`](../docs/architecture/v2-authentication-authority.md)).
There is no auth-disabled mode, no legacy scoped key, no delegated JWT, and no
CRD-based customer authority.

- **Browser sessions** reach the work UI and every cookie-only security and
  consequence-capable command. Unsafe methods require the exact public origin
  and the session-bound `X-CSRF-Token`. An Operator session holds every
  non-Admin personal action; an Admin session holds the complete personal
  catalogue. Role and account state are re-read on every request.
- **V2 API credentials** (personal or automation; lifecycle in HOR-514) hold an
  immutable subset of the fixed action catalogue and are resolved live on every
  request; a bearer request never falls back to a cookie.
- Every HTTP route is registered through `Handler.route` with exactly one class
  (`probe`, `public`, `session`, `action`, `bearer_action`), and
  `TestEveryRouteIsClassified` fails on an unclassified or reclassified route.
- `POST /v1/work-items` records the initiating human, request actor, and
  authorization evidence from the authenticated boundary; work list, detail,
  start, revision, and dashboard responses clip value data unless the
  principal holds the Admin-only `value.read`.

**Bootstrap** (`control-plane-api bootstrap`, the RBAC-less init container
after `migrate up`) runs once per installation, keyed on the bootstrap marker:
it makes the overlay-named first Admin a `setup_pending` Admin (converting an
existing account with that email) and queues a one-time setup link through the
configured mail relay. It never creates or prints a credential.
`--recover-admin` is the cluster-operator path when no active Admin exists. See
[`docs/browser-auth-operations.md`](docs/browser-auth-operations.md).

The `permissions.effective_capabilities` view now grants the broad workload
capability only to non-human workload identities (workflow scope identities);
the inference gateway reads it for the workload mTLS path.

## Tool gateway (HOR-392)

The tool gateway is the authorized boundary for customer-system and externally
side-effecting tool execution (ARCH-001..ARCH-018). It runs as `cmd/gateway`, a
third entrypoint in the `control-plane` image, serving the `iterabase.gateway.v1`
gRPC contract over native HTTP/2 + mTLS (a SPIFFE URI SAN binds caller identity):

- **`RunnerService.RegisterRunner`** — a long-lived bidi stream. Trusted tool
  runners connect outbound, self-register immutable `ToolDescriptor`s (name /
  version / digest / JSON input schema / effect class / credential slots /
  artifact capabilities / timeout / write-consequence summary template),
  heart-beat availability, and receive
  invocations to execute. Runners expose no inbound endpoint (ARCH-015).
- **`GatewayService`** — the caller-facing surface. `DiscoverEffectiveTools`
  returns only the descriptors permitted for the caller's active attempt/turn or
  workflow-step context (registered healthy versions ∩ AgentPool grants ∩
  workflow-requested tools ∩ pool-level identity scope). `InvokeTool` is the
  ledger-gated path: the gateway commits an invocation row **before** the
  side-effect boundary, then dispatches over the runner stream.

The durable at-most-once ledger (`toolgateway.invocations`) is uniquely keyed by
attempt + caller scope + tool-call id + exact version digest + idempotency key.
Duplicates of a completed invocation return the committed result; duplicates in
progress report the existing invocation; a possible effect with no committed
result becomes `outcome_unknown` and is never automatically repeated. Retry is
classified by effect class: `read_only` may bounded-retry; `idempotent_write`
only when the descriptor proves a stable strategy; `non_idempotent_write` never
auto-retries after execution begins (ARCH-014).

Every write descriptor must also provide immutable English and Portuguese
consequence templates. Placeholders bind only to explicitly allow-listed RFC
6901 argument paths. After schema/resource validation, the gateway renders and
persists the localized customer-safe summary with the invocation **before**
dispatch; missing, structured, or unrenderable fields fail closed. Cycle and
revised-attempt confirmation return that exact persisted summary and never raw
arguments/results, including when the write becomes `outcome_unknown`
(ARCH-022 / REQ-026).

The gateway resolves logical credential slots to K8s Secret values via the
in-cluster API (Secret-read RBAC scoped to `gateway.kube_namespace`) and hands a
short-lived `CredentialContext` to the trusted runner over mTLS. Raw values
never enter Postgres, logs, tool results, or the sandbox (ARCH-008). For
`oauth_client_credentials` slots the gateway alone holds the client secret and
performs the grant; the tool receives only the short-lived access token as
`credentials.<slot>.accessToken` (bearer slots use `credentials.<slot>.value`).
Large
inputs/outputs are `ArtifactRef`s (the MinIO-backed artifact service is
HOR-399).

AgentPool and Workflow reconcilers populate grants, bindings, and immutable
attempt pins. Exact runner identities and allowed tool namespaces come from
static gateway deployment configuration and are reconciled into
`approved_runners`; runners cannot approve themselves.

### Tool generations (HOR-397, HOR-651)

`tool-runner/` builds the separate generic Node 24
`control-plane-tool-runner` image. The release-versioned product tool layer
(`tool-runner/tools/product`) is baked into that image and released with the
control-plane (DES-HOR-651-01). A materializer sidecar composes each generation
from that product layer plus, optionally, the `tools/client` layer of one
configured Flux `GitRepository`. It verifies the exact artifact digest, safely
extracts only `tools/client`, rejects an overlay `tools/product`, and atomically
publishes immutable generation directories named by both layer digests. The
runner mounts those files read-only, validates the complete manifest/bundle set,
and registers over outbound mTLS. It receives no Kubernetes or Git credential;
the materializer receives no runner private key, and with
`toolRunner.flux.enabled=false` no Kubernetes credential either. Both
processes expose unauthenticated, metrics-only `/metrics` listeners for the
chart's opt-in Prometheus `ServiceMonitor`; no invocation API is exposed. See
[`tool-runner/tools/README.md`](tool-runner/tools/README.md) for the
product/client layer contract.

A client bundle shadows a product bundle by logical name only with a new
immutable version. The digest binds canonical manifest metadata and exact ESM
bytes; mismatches reject the whole revision while the last valid generation
continues serving. Superseded registrations enter coordinated drain: new
attempts cannot select them, existing pins remain routable, and release occurs
when no unfinished pin remains or the configured hard retention deadline makes
them explicitly unavailable. No substitution occurs. A platform upgrade replaces
the runner pod; attempts pinned to a product version that the upgrade
superseded keep their pins and receive the retryable `tool_unavailable`
(DES-HOR-651-02).

Use `make tool-runner-lint tool-runner-test tool-runner-build` locally;
`tool-runner-test` also validates every product tool and enforces product
version immutability against `master`. The image provides `digest
<tool-directory>` and `validate [overlay-directory]` authoring commands. Concrete Graph tools remain owned
by HOR-358; the supervisor IPC client is HOR-395.

## Model backends (HOR-306/388)

A `ModelBackend` (`kind: vLLM`) deploys an internal GPU workload (Deployment +
Service) and materializes into `catalog.backends`; a `Model` references it and
carries the client-facing alias/config. For **plug-n-play** models the
controller assembles the serving command (`--model <id> --port --host
<extraArgs>`) and manages `HF_HOME=/data/hf-cache`, a sized `/dev/shm`
(`devShmSize`, default 2Gi), the `nvidia` runtimeClass, the GPU
request, the GPU node selector, and a 10-min startup probe.

**Custom vLLM builds** (HOR-388) — e.g. a quantized/SM120-specific build with a
non-stock entrypoint, build-tuning env vars, or runtime patch overlays — use the
generic corev1-passthrough escape hatches. None of these affect the plug-n-play
path when unset:

| Field | Effect |
|---|---|
| `spec.command` / `spec.args` | Override the container ENTRYPOINT/CMD. When `args` is set, the controller **skips** its `--model/--port/--host` assembly and `extraArgs`; the deployer owns the whole command shape (e.g. positional `vllm … serve <model>`). `healthProbe.port` stays the sole port source for the Service + probes — the deployer must bind `--port <healthProbe.port>` in `args`. |
| `spec.env` | Extra env vars (`corev1.EnvVar`, supports `valueFrom` for `HF_TOKEN` from a Secret). The controller injects `HF_HOME=/data/hf-cache` only if `env` doesn't already set it (user wins). |
| `spec.persistentVolumes[]` | Generic controller-owned serving claims. Every entry declares a unique stable name, unique absolute mount path, explicit `iterabase-lvm-xfs`, and positive desired size. Claims are deterministic RWO/Filesystem PVCs; identity is immutable after provisioning and size may only grow in place. `/data/hf-cache` backs the default `HF_HOME`; `/cache` remains a generic mount with no LMCache-specific behavior. |
| `spec.volumes` / `spec.volumeMounts` | Extra volumes/mounts, appended after managed persistent/ephemeral storage and `dshm`. Reserved names and mount-path collisions are rejected. Use for runtime file-artifact overlays such as a ConfigMap of patch files. The unrelated disposable `container-tmp` hostPath remains allowed and is not HF/cache persistence. |
| `spec.hostIPC` | Opt-in `hostIPC` (default false). Sized `/dev/shm` (`devShmSize`) is the normal TP shm mechanism; flip on only if a build's NCCL/CUDA IPC proves to need it. |
| `spec.securityContext` | Container-level `corev1.SecurityContext` passthrough. Set `capabilities.add: [IPC_LOCK]` for NCCL TP — `CAP_IPC_LOCK` bypasses `RLIMIT_MEMLOCK` (the K8s equivalent of `--ulimit memlock=-1`; a bash `ulimit` wrapper is a no-op without `CAP_SYS_RESOURCE`). Also covers `runAsUser`, `readOnlyRootFilesystem`, etc. |
| `spec.healthProbe.startupTimeoutSeconds` | Scales the startupProbe window (default 600s; controller renders `period=10, failureThreshold=ceil(n/10)`). Raise for large/long-context models whose warmup is slow. |

The platform does **not** carry forked serving images: a custom build consumes
its upstream image verbatim and overlays any patches as file artifacts
(ConfigMaps). A backend with a managed `/data/hf-cache` declaration persists
weights on its controller-owned PVC; an undeclared HF cache uses only an
`emptyDir` and remains plug-and-play but ephemeral. Set `HF_TOKEN` in `env` for
gated models. See
`config/samples/platform_v1alpha1_modelbackend_dsv4flash_b12x.yaml` for a full
B12X/SM120 example (DeepSeek-V4-Flash NVFP4, TP=2, MTP, 256k context, patch
overlays). Multi-replica TP/PP and SGLang are deferred (HOR-323 / deepen).

## Agent pools (HOR-245)

An `AgentPool` is the deployable workflow security/integration boundary
(ARCH-018): it provisions a bounded set of isolated warm-worker pods and
declares the maximum gateway capability grants + logical credential-slot
bindings for work dispatched to the pool. No separate Tool/EgressRoute/
IntegrationBinding CRD exists in v1.

The operator reconciles, per pool, one explicit Filesystem-mode **ReadWriteOnce
sandbox PVC** on `iterabase-agentpool-lvm-xfs`. `spec.sandbox.size` is desired
grow-only thick XFS capacity in the fixed `iterabase-data` VG. An increase
patches only that claim; shrink, identity change, copy, and replacement are
refused. During online growth, readiness and fresh dispatch authorization stay
closed while mounted workers remain available for Kubelet XFS expansion and an
active turn is not aborted solely by resize. Reopening requires PVC/PV/OpenEBS
convergence and a fresh valid mounted-filesystem `statfs`. All trusted root supervisors in that pool may access
the whole claim; separate pools receive separate OpenEBS volumes. Model-directed
children use stable distinct session UID=GID, cleared groups, no capabilities,
`no_new_privs`, umask `0077`, and session-owned `0700`
root/home/tmp/session/workspace beneath a root-owned `0711` PVC root. RWO limits
a volume to one node, not one pod; OpenEBS `shared: yes` permits two or more
same-pool workers to mount it on the supported single K3s node without creating
RWX or cross-node support.
It also reconciles a **deny-by-default NetworkPolicy** (`denied` = kube-dns + the three gateways;
`internet` = per-pool opt-in for non-cluster egress — customer-system
credentialed access still routes through the gateway), a **per-pod config
ConfigMap** (rendered harness boot config), and the **warm-worker pods**.

**Per-pod SPIFFE certs** are issued by the **cert-manager CSI driver**
(`csi.cert-manager.io`) annotated on each pod with URI SAN
`spiffe://<td>/pools/<pool-uid>/workers/<pod-name>`; the operator never holds
the CA key (cert-manager owns the CA in `platform-ca`, backed by a
`ClusterIssuer`). Forge prerequisites: cert-manager + cert-manager-csi-driver +
the CA `Certificate`/`ClusterIssuer`. The supervisor runs as root (UID 0, PSS
`baseline`) to validate/read only the CSI driver's exact contained AtomicWriter
chain and root-owned regular exact-`0440` resolved target, the narrow fixed-mode
upstream exception, and launch the per-turn child as the session UID via `setpriv`.
The rendered supervisor retains runtime-default capabilities and explicitly adds
`SETUID`/`SETGID` (which PSS `restricted` forbids); no `fsGroup` grants access.
The child uses equal stable UID/GID, cleared groups, no capabilities,
`no_new_privs`, and umask `0077`, so opening the key returns `EACCES`.

Readiness requires the exact non-default, grow-only-expandable
`iterabase-agentpool-lvm-xfs` class (`local.csi.openebs.io`, XFS,
`vgpattern: ^iterabase-data$`, thick, `shared: yes`, `WaitForFirstConsumer`,
`Delete`), one explicit RWO Filesystem claim, and a bound CSI PV whose driver,
filesystem, VG attribute, volume handle, local node affinity, Ready LVMVolume,
and LVMNode VG identity agree. The initial unbound `WaitForFirstConsumer` state
does not deadlock worker creation. Wrong/ambiguous class, CSI, filesystem, VG,
OpenEBS identity, topology, access/volume mode, unauthorized/non-size claim
mutation, unsafe ownership, or actual I/O failure fails closed. Positively
observed identity/topology corruption retains destructive quiescence; safe
resize and unknown observations retain mounted workers while closing credit.

The harness measures available blocks on its pool's actual XFS PVC and persists
that pool's local hysteresis state on the claim. Dispatch serializes a separate
Postgres-backed gate for each materialized AgentPool, exports metrics labelled by
pool, and the manager projects only the matching state into
`WorkspaceCapacityHealthy`. One pool warns below 25%, withholds all of its fresh
credits at or below 20%, retains the gate across worker/pool/dispatch
replacement, and reopens only at or above 25%; unrelated pool PVCs remain
independent. Crossing the threshold alone does not abort an active turn; after
its terminal event is ACKed, the next credit remains withheld. Zero space or a
real I/O/fsync/mount failure fences through the existing worker-loss path
without automatic turn/effect replay. Chart-owned OpenEBS `lvm_vg_*` metrics
separately expose aggregate `iterabase-data` pressure and honest new-claim
exhaustion.

`spec.workspaceTools` is the deny-by-default local-tool switch (ARCH-016):
`false` exposes none; `true` exposes exactly `read`/`write`/`edit`/`bash`.
`spec.gatewayGrants`/`credentialBindings`/`resourceConstraints` declare the
maximum gateway permissions and slot→Secret bindings (values never in the CRD);
semantic tool-registry validation is the gateway's job (HOR-392/397). Dispatch,
warm-pool scaling, and the Work server's worker-identity/generation fencing are
HOR-249; `worker_id` = pod name (stable slot), recorded as an amendment to the
HOR-381/249 identity contract. Real-cluster PSS/OpenEBS LVM RWO/isolation validation is
the ticket's stated real-cluster gate (envtest covers assembly + structural
validation).

## Dispatch (HOR-249)

The dispatch Work server (`cmd/dispatch`) is the native-gRPC mTLS endpoint warm
workers connect to (`iterabase.harness.v1.Harness.Work` bidi stream). It owns
worker fencing, one-credit dispatch, durable `TurnEvent` ACK/dedup, cancellation
and worker-loss semantics, and the dispatch reconciler that drives pending
runs/steps/turns to eligible idle workers.

A worker connects (Hello), the server verifies the cert SAN (pool UID + pod name)
against Hello and sends a `Welcome` with a fresh fencing generation. A reconnect
for the same (pool, worker) fences the prior generation and terminalizes its
active turn as worker loss (aborted). `Ready` advertises one dispatch credit; the
reconciler assigns a pending turn (whose run is durably assigned to the worker's
pool via `runtime.run_pool_assignments`) to an idle worker and sends
`AssignTurn`. `TurnEvent`s are committed to the append-only `runtime.events` log
with per-turn sequence dedup and cumulatively ACKed; a terminal `WorkerOutcome`
terminalizes the turn/step/run (the CP is first-terminal-writer; a late outcome
is after-terminal audit only). Cancellation sends `AbortTurn` and terminalizes
the turn aborted; an ambiguous non-idempotent turn is never auto-redelivered.

The active-assignment context (`runtime.turn_assignments`) is the durable record
binding a running turn to the specific verified worker (cert SAN pod name) +
current fencing generation under one AgentPool, with the immutable
attempt/model/capability/tool-version snapshot. The tool gateway (this repo,
`ResolveTurnScope`) and the inference gateway (cross-repo, HOR-398) read it
fail-closed to deny a still-valid supervisor cert from a different same-pool
worker or a fenced/old-generation caller for a running turn — closing the
HOR-398 / DEC-041 interim residual.

Trigger adapters remain separate slices: email (HOR-356) and operator-artifact
(HOR-393) call the idempotent work-start API. HOR-254's graph service prepares
the active node; `cmd/dispatch` assigns only `agent_task` nodes to workers while
`human_gate`, consequence-confirmation, and `wait` nodes remain durable
control-plane state that holds no worker. Each reconcile pass also routes
expired waits through `deadline` and derives `runner_offline` pause conditions
from runner heartbeats. A lost graph turn (stream loss, lease expiry, reconnect
fencing, ambiguous send) is classified by the Tool Gateway ledger
(DES-HOR-649-04): with no write invocation its step reruns as a new visit in a
fresh session, otherwise the attempt stops. Worker fencing/ACK semantics remain
the HOR-249 boundary.

## CRD landscape

All CRDs use group `platform.iterabase.com` / `v1alpha1`, reconciled by this
operator: `AgentPool` (HOR-245), `ModelBackend` (HOR-306), `Model`
(HOR-268), and `Workflow` (HOR-252, **defined here**). `IdentityMapping` and
`PermissionPolicy` retired with the V2-only authority (HOR-582).

## Workflow definitions (HOR-252)

A `Workflow` is an operator-defined, versioned customer operational workflow
(REQ-001). The operator deploys one from product/client overlay artifacts:
its source adapter/trigger, exact immutable skill references, default/per-node
model selection, requested gateway capabilities, customer-facing presentation,
value-model reference, and a single-active-node directed graph.

- Definitions are **immutable per version** (ARCH-007): `(key, version)` is the
  unique version identity. A content change must be published under a new
  `spec.version`; the reconciler rejects a content change under an
  already-registered version. Re-registering the same `(key, version, digest)`
  is idempotent; distinct versions may share a content digest. Every version of
  one logical `key` retains the same durable workflow scope identity, so latest-
  version resolution cannot cross workflow/customer ownership boundaries.
- **Validation runs before execution**: unknown source/model/node kind,
  capability/skill narrowing, malformed JSON Schema, missing/duplicate outcome
  route, unreachable node, non-terminating component, or binding fails with an
  inspectable `validationStatus: invalid`. v1 source types are `graph_email` (Walter),
  `operator_artifact` (XBS), and `manual_api` (API-backed operator source, ARCH-026) —
  no customer-specific schema — and a recognized type becomes Ready only when its
  adapter is installed in the deployment.
- `spec.graph` declares one entry node, `agent_task`/`human_gate`/`wait` nodes,
  outcome-routed edges, explicit terminal outcomes, and a bounded transition
  count. Cycles are supported; parallel branches/joins are not. Every visit is
  append-only. Gateway tools are agent-callable capabilities, never graph nodes.
- A `wait` node (HOR-649) collects correlated external events without holding a
  worker: `wait.events[]` names a `source` and a `correlation` output path of a
  dominating earlier node (string or array of strings), `completion` is
  `all`/`any`, and `deadline` is `after` (1m–90d) or `at` an RFC3339 output
  field. Its reserved outcomes `received`, `deadline`, and `cancelled` must each
  be routed. Event sources post through `work.Store.PostExternalEvent`, keyed
  idempotently by `(source, eventKey)`. See
  [`docs/architecture/v2-parallel-cancellation-safe-restart.md`](../docs/architecture/v2-parallel-cancellation-safe-restart.md)
  section 24.
- Every agent node finishes through the reserved `complete_step` control
  function. Its declared outcome and JSON output are validated before the
  engine atomically follows an edge. A clean worker outcome is also required.
  Every terminal node defines `resultPresentation`: localized EN/PT receipt
  summaries per terminal outcome plus localized property-path and controlled-
  value labels aligned exactly with its closed structured-result schema.
  Terminal agent tasks may declare that closed direct-object `outputSchema`
  when they produce a structured result; omitted/`{}` terminal tasks are
  artifact-only. Any declared schema must be presentable, so an
  unrepresentable root shape is still rejected. The presentation is snapshotted
  on each node visit so historical results retain customer-readable identity
  and hierarchy without exposing machine keys.
- A workflow **cannot request capabilities beyond its `poolRef` AgentPool**:
  each requested tool must be granted and its effect/action narrowing must not
  exceed the pool grant; every node exposes an explicit subset, enforced from
  the durable turn assignment through gateway discovery/invocation.
- **Trigger bindings are source-specific typed, non-secret routes** (`graphEmail.mailboxAddress`
  or `operatorArtifact.sourceID`). There is no opaque trigger config/secret
  persistence path; credentials resolve through AgentPool `credentialBindings`
  via the gateway (ARCH-008).
- The reconciler materializes: an immutable `workflow.definitions` row,
  non-secret `workflow.trigger_bindings`, a `kind=workflow` scope identity
  (HOR-242), and a `toolgateway.workflow_pool_binding` (permitted tools). The
  `definition_key` wire format `<key>:<version>` is the stable cross-schema
  reference stored in `runtime.workflow_runs.definition_key` (HOR-246) and
  `toolgateway.workflow_pool_bindings.workflow_definition_key` (HOR-392).
- `ResolveForAttempt(key, version)` returns the exact versioned canonical
  workflow/config, immutable skill identities, typed trigger bindings, scope
  identity, and complete capability narrowing for attempt creation (HOR-254
  atomically composes it with deterministic latest-healthy tool pinning).
  Skill digests are `sha256:<hex>` over the complete materialized skill tree:
  for each sorted slash-separated relative file, hash
  `file\\0<path>\\0<byte-length>\\0<exact-bytes>`; symlinks and non-regular
  entries are rejected. The harness loads a named
  product/client skill only when those bytes match the attempt's pinned digest.
- Customer-facing metadata (workflow title, persona name/avatar, locale)
  supports the single-workflow Dashboard **without a separate Persona CRD**
  (REQ-021; HOR-363 canceled).

See `config/samples/platform_v1alpha1_workflow.yaml` for Walter `graph_email`
and XBS `operator_artifact` examples.

## Durable customer work (HOR-254)

One customer work item owns one or more attempts; each attempt shares its UUID
with exactly one `runtime.workflow_run`. Current To do / In progress / Waiting /
Paused / Blocked / Done / Failed presentation is projected from the current
attempt plus its open actionable blocker, open pause, and open wait, avoiding a
duplicate mutable state machine. Waiting and Paused are never Blocked; the
Dashboard shows them in the In progress column with their own state chip.
Failed remains outside the four normal columns. A pause holds the next step of
matching work while a live `work.pause_conditions` cause (`trial_ended`,
`provider_unavailable`, `runner_offline`) applies, and resumes it automatically
when the cause clears.

Work APIs require a `work`-scope bearer key and operation capability
(`work:read`, `work:start`, `work:respond`, `work:feedback`, or `work:retry`):

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/work-items` | Idempotent workflow start (`Idempotency-Key` required) |
| `GET` | `/v1/work-items` / `/v1/work-items/{id}` | Search/list and customer-safe detail |
| `GET` | `/v1/work-items/{id}/attempts` | Preserved attempt history |
| `GET` | `/v1/work-attempts/{id}/nodes` | Append-only graph-node visits and structured outputs |
| `GET` | `/v1/work-items/{id}/timeline` | Customer-safe business timeline |
| `GET` | `/v1/work-items/{id}/artifacts` | Immutable item/attempt artifact links and download metadata |
| `GET` | `/v1/work-items/{id}/consequences` | Customer-safe external-action tokens required by a revision |
| `GET` | `/v1/work-items/{id}/blocker` | Current actionable request |
| `POST` | `/v1/work-blockers/{id}/responses` | Resolve human/consequence gate |
| `POST` | `/v1/work-items/{id}/wait/stop` | Stop the open wait and continue through its `cancelled` outcome (cookie session + CSRF only, like blocker responses) |
| `GET` | `/v1/work-items/{id}/feedback` / `/v1/work-items/{id}/feedback/{feedbackID}` | Original-attempt feedback and revised-attempt association |
| `POST` | `/v1/work-items/{id}/feedback` | Save feedback without starting work |
| `POST` | `/v1/work-items/{id}/revisions` | Explicit revised attempt with exact consequence confirmation |
| `GET` | `/v1/work-dashboard` | State counts plus explainable estimated value/trend |
| `GET` | `/v1/work-events` | Resumable SSE (`Last-Event-ID`) of business-safe events |
| `POST` | `/v1/value-models` | Admin-only immutable value-model version creation |

Work-item creation separates private `source` trigger context from required
`sourcePresentation` (`kind`, business title/subtitle, optional HTTPS original
link, and localized evidence). Customer APIs serialize only the latter. The
authenticated `POST /v1/work-items` endpoint is also the installed adapter for
the `manual_api` Workflow source (ARCH-026): it is used for controlled operator
starts of a Ready `manual_api` Workflow, enforces `work:start` and a mandatory
`Idempotency-Key`, bounds request/source payloads, and does **not** prove Graph
email ingestion (that remains `graph_email`, unavailable until its HOR-356
adapter is installed). It has no uploaded form, customer trigger UI, or public
unauthenticated route. Each
workflow node requires English and Portuguese business labels. Human gates
also require ordered localized labels for outcomes, response fields, and enum
options; blockers snapshot that presentation so the Dashboard never exposes
machine identifiers. Their v1 response schemas are constrained to the exact
direct object/property contract the Dashboard can encode: top-level fields may
use string, boolean, number, integer, unconstrained object/array JSON, or enum;
unsupported validation keywords (including nested schemas, patterns, ranges,
and composition/references) and indirectly declared required fields are
rejected before deployment.
Attempts and node visits snapshot workflow/persona/stage presentation for
stable cards. Terminal visits additionally snapshot localized result summaries,
structured property-path labels, and exact enum-value labels; the Dashboard renders that
contract rather than agent summaries, stage labels, or raw schema identifiers.
Artifact blocker responses accept validated
`artifactRefs`, link them to the
human-gate visit, and propagate them as authorized inputs when work resumes.

The private Dashboard is served at `/`. The operator supplies a work-scope API
key after loading the page; the key is retained only in JavaScript memory and
is never put in a URL, cookie, local storage, or session storage. Walter access
remains SSH port-forwarding/screen sharing per the Platform-v1 release boundary.

`runtime.events` and gateway ledgers remain technical/operator records and are
never returned by customer work APIs. `work.timeline_events` is append-only and
contains only semantic business codes/parameters. HOR-399 owns artifact bytes
and metadata; HOR-254 stores immutable references and trace relationships.

## Shared artifacts and quarantine (HOR-399, HOR-461)

The installation is the tenant boundary. The contract is
[`docs/architecture/v2-artifact-processing.md`](../docs/architecture/v2-artifact-processing.md).
Every source (browser/API upload, mailbox, runner and tool output, workflow,
sandbox publication) enters **one quarantine path**:

- `artifact.artifacts` keeps independent `storage_state`, `security_state`,
  `extraction_state`, and library registration, plus immutable identity,
  source, lineage (`parent_artifact_id` for email children), and attribution.
  `artifact.is_cleared(id)` is the database authority every consumer joins on
  immediately before using bytes.
- MinIO zones are policy-separated prefixes. api/gateway hold only the ingest
  (quarantine write-only, create-only) and serve (cleared read-only) keys; the
  `artifact-coordinator` alone holds the lifecycle key.
- The coordinator leases fenced security attempts (`FOR UPDATE SKIP LOCKED`;
  attempt 1 now, 2 after one minute, 3 after five minutes), streams the exact
  quarantine object over HTTP/2 mTLS to the `artifact-scanner` adapter, admits
  `.eml`/`.msg` children with lineage, promotes the exact digest into the
  cleared zone, registers it for search, and purges rejected, deleted,
  expired, and orphaned bytes.
- The scanner adapter (no data credential, no egress) asks the pod-local
  ClamAV `clamd` for a verdict, then applies the Go admission policy in
  `internal/artifact/admission` (DES-HOR-461-01). It is ready only while
  ClamAV passes an EICAR self-test on signatures checked within 24 hours.

Customer routes (exact V2 actions, `internal/server/artifact_api.go`):

| Method | Path | Authority | Behaviour |
| --- | --- | --- | --- |
| `POST` | `/v1/artifacts` | `artifacts.upload` | Raw stream, `Idempotency-Key` and `X-Artifact-Filename` required, 25 MiB; `202 {uploadId, state}` provisional handle, never a usable ref |
| `GET` | `/v1/artifacts?search=&cursor=&limit=` | `artifacts.read` | Registered, cleared artifacts only; recent order or PostgreSQL full-text search |
| `GET` | `/v1/artifacts/{id}/status` | `artifacts.read` | Handle or id; unregistered rows only for the initiating human, the exact automation key, or a browser Admin; canonical ref only once cleared |
| `GET`/`HEAD` | `/v1/artifacts/{id}` | `artifacts.read` | Cleared original only, `attachment` + `nosniff`; anything else is `404` |
| `POST` | `/v1/artifacts/{id}/security-retries` | browser session + CSRF | Initiating human or Admin; one new three-attempt trigger after transient failure |
| `DELETE` | `/v1/artifacts/{id}` | personal Admin key with `artifacts.read` + `artifacts.delete` | `409 artifact_in_use` while active work references it; otherwise `202` and an asynchronous purge to a content-free tombstone |

The gateway mTLS listener exposes `PutArtifact` (into quarantine; returns the
immutable ref with state `checking_security`), `GetArtifact`, and
`StatArtifact` (cleared only). A producer may cite its own quarantined output
in `complete_step` or a tool result; every consumer waits for clearance, and a
node is not assigned until its inputs clear (DES-HOR-461-02). No caller
receives MinIO credentials or presigned URLs.

`AssignTurn.materializations` is the exact authorized, cleared input set. The
trusted supervisor downloads and verifies size/digest before starting the
child. The reserved `publish_artifact` control function validates a regular
relative workspace path, rejects traversal/symlink escape, and streams a new
quarantined artifact (with its filename) through supervisor IPC.

Security advisories use `control-plane-artifact-coordinator rescan --reason
<code> <artifact-id>...`: it withdraws each artifact from search and every read
and queues an exact re-scan. Operations: [`../charts/docs/artifact-operations.md`](../charts/docs/artifact-operations.md).

## Git workflow

Branches/commits/PRs carry the Linear ticket ID (e.g. `HOR-241`). See
`AGENTS.md`. Only the user approves/merges to `master`.
