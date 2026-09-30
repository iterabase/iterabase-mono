# harness

The Node pi harness worker that runs in the AgentSandbox — **the agent**
(HOR-381). A **warm trusted supervisor** process maintains one long-lived mTLS
gRPC `Work` bidi-stream to the control-plane (HOR-249) and, per turn, spawns a
**disposable pi child** under a per-session UID/GID (via a `setpriv` launcher)
that runs one turn, streams durable lifecycle events, and exits. Per-customer,
fully isolated, self-hosted. See the Platform Direction doc (Obsidian:
*Horizonshift Platform Direction*) §4/§7/§9 + the HOR-381 plan note for the
architecture; this directory is the source of truth for the harness
implementation intent.

Supersedes the HOR-351 "boot-bound-to-one-session Connect server" (Prompt/Abort)
— the worker is now the mTLS gRPC **client** (no inbound RPC); per-turn
stateless (the process is warm; only the per-turn child + its in-memory session
are per-turn).

## Architecture

```
worker pod
├── supervisor (long-lived, trusted; dist/main.js)         UID 0; runtime-default caps + explicit SETUID/SETGID (PSS baseline)
│   ├── mTLS Work bidi-stream client (worker=client, CP=server)
│   ├── protocol state machine (single-credit invariant) + heartbeat
│   ├── event outbox + WAL (durable audit; no tail loss on crash)
│   ├── probes (/healthz, /readyz) + SIGTERM drain
│   └── child-process.ts: spawn the child via the setpriv launcher + IPC
└── pi child (one assignment; dist/child.js)               stable UID=GID, no groups/caps, no_new_privs, umask 0077
    ├── fresh AgentSessionRuntime (resume-or-create from the PVC)
    └── pi AgentSessionEvent -> durable TurnEvent payloads (stdout IPC)
```

The supervisor never imports pi/extensions/tools — it talks to the `Child`
abstraction. The child is the only place model-directed code runs, under the
session UID with kernel-enforced filesystem isolation.

## Contract (`proto/iterabase/harness/v1/harness.proto`)

Native gRPC over HTTP/2 + mTLS (SPIFFE URI SAN binds Pod/pool identity). One
bidi method: `rpc Work(stream WorkerMessage) returns (stream ControlMessage)`.

- **Worker→CP:** `Hello` (cert-SAN-bound), `Ready` (one dispatch credit),
  `Heartbeat`, `TurnEvent` (durable, sequenced, cumulatively ACKed),
  `TokenDelta` (ephemeral, non-sequenced — live token streaming).
- **CP→worker:** `Welcome` (fencing generation + lease intervals),
  `AssignTurn` (per-turn session/persona/model/workspace-tools/run-id/sandbox/message plus exact artifact materializations),
  `AbortTurn` (idempotent), `EventAck` (cumulative, post-Postgres-commit),
  `SessionEnd` (session terminated → supervisor reaps the per-session sandbox;
  legal only when no turn is active).

12 durable `TurnEvent` payloads (`ExecutionStarted`, `ModelCallStarted`,
`AssistantMessage`, `ModelCallFailed`, `ModelRetryScheduled/Finished`,
`ToolCallStarted`, `ToolResult`, `CompactionStarted/Finished`, `HarnessError`,
`WorkerOutcome{COMPLETED|ABORTED|FAILED}`). `Steer`/`FollowUp` + CP-side
token-delta UI forwarding are deferred (interactive surfaces).

## Sandbox

`/data/sandboxes/<sandbox-id>` is the session root; it and its canonical
`home/tmp/session/workspace` children are `0700`, owned by one stable distinct
CP-assigned UID equal to its GID. `/data/sandboxes` is root-owned `0711`
(traversable, not listable). Every trusted root supervisor in one AgentPool may
access that pool's whole PVC; separate AgentPools have separate claims/mounts.
Under the HOR-381 provisioning rescope (founder-approved 2026-08-02), the
**supervisor itself provisions** the per-session sandbox at `AssignTurn`
(`provisionSandbox` creates the `0700` entries chowned to the session UID/GID,
then `validateSandbox` is the post-provision integrity gate); it remains
validate-only for an **existing** path (never chowns/"completes" a mismatched
or partial sandbox — missing/mismatched → typed `FAILED`) (+ repo CoW checkouts
for agentic coding). On `SessionEnd` (HOR-245 cleanup contract) the supervisor
reaps `<sandbox-id>/` after verifying non-symlink + session ownership. Reuse-
safety: the CP MUST NOT recycle `sandbox_id` or `(uid, gid)` before reaping
(v1: no reap-ack; fail-closed reap + bounded grace; a stream loss before
`SessionEnd` is handled leaks until reconciled — accepted v1 limitation). The
child runs with equal session UID/GID, cleared supplementary groups, all
capabilities dropped, `no_new_privs`, and umask `0077` (kernel `EACCES` for
known sibling session roots). Extension code is pool-bound read-only; the
per-turn `workspaceTools` switch (ARCH-016) exposes exactly pi's built-in
`read`/`write`/`edit`/`bash` when enabled — no arbitrary local-tool catalogue.

HOR-538 adds a runtime storage-health transaction every ten seconds under the
root-only `.iterabase-storage-health` directory: revalidate mount-root
ownership/mode and actual-filesystem available blocks, then write, `fsync`,
atomically rename, directory-`fsync`, and unlink a worker-scoped marker. A
capacity, mount, ownership, or I/O failure removes probe readiness/health,
drains the Work stream, and exits the worker. Dispatch fences worker loss;
neither the supervisor nor operator silently replays a turn or effect.

HOR-399 materializes only the canonical artifact references carried by
`AssignTurn`, before child startup, under the session UID/GID. Size and SHA-256
must match the service metadata or the turn fails. The reserved
`publish_artifact(relative_path, mime_type)` platform control function crosses
fd 4/fd 5 to the supervisor, which opens a regular file without following the
final symlink, verifies the opened fd still resolves beneath `workspace/`, and
streams it through the gateway ArtifactService. It is not a fifth AgentPool
workspace tool. The child receives no MinIO credential, URL, endpoint, or
network route.

## Config (`/etc/harness/config.yaml`, ConfigMap-mounted by HOR-245)

**Infra-only at boot** — no persona/model/session/tools (those are per-turn via
`AssignTurn`): control-plane gRPC URL + expected server name, worker Pod UID +
pool UID, optional pool scope identity, mTLS cert/key/CA paths, sandbox mount
root, read-only `piDirs`, the **tool-gateway** + **inference-gateway** mTLS
endpoints (ARCH-010 — separate domain gateways, one worker SPIFFE cert), WAL
spool dir (emptyDir), probe port, + HTTP/2 ping / reconnect / child-liveness /
abort-grace / outbox-bound / model-retry / token-delta-buffer tunables. Certs
are re-read each reconnect (rotation). Before startup and on the bounded health
cadence, the supervisor observes (never repairs) only the exact cert-manager CSI
AtomicWriter chain beneath non-child-writable mount/ancestors, safely resolves the
current root-owned regular exact-`0440` target, and proves it readable; `0440` is
the narrow fixed-mode upstream exception. Drift withdraws readiness/health and
drains fail-closed. The supervisor holds the only usable
mTLS credential; the disposable child receives `EACCES` opening the mounted key
and has no direct network route (ARCH-003) — model/tool traffic crosses
dedicated IPC channels (fd 4/fd 5) to the supervisor, which opens the authenticated upstream
streams (ARCH-011).

## Failure semantics

- **Durable audit, no tail loss on crash:** durable `TurnEvent`s are fsync'd to
  a local WAL before send; a supervisor *crash* loses no audit tail (on restart
  the WAL is replayed as `after_terminal` — the CP already terminalized the turn
  as worker-loss via fencing). Pod-death/per-worker-PVC survival is deferred.
- **Stream loss (supervisor survives):** fail-closed — abort the child, never
  resume; the unacked tail is replayed as `after_terminal` on reconnect.
- **Non-idempotent turns:** no auto-retry; a failed turn is a workflow decision
  (HOR-246/HOR-249). Tool calls carry `turn_id + tool_call_id` as audit/correlation.
- **Cancellation:** CP `RUNNING→CANCELED` CAS fences immediately; `AbortTurn` is
  best-effort; the worker's late outcome is `after_terminal` (first-terminal-writer).
- **Outcomes:** `COMPLETED` only after `agent_settled` + flush + `session_shutdown`
  + dispose + clean exit + ACK; a successful message + failed cleanup = `FAILED`.
- **Child liveness and abort diagnostics (HOR-551):** while turn work is in
  flight the supervisor aborts a child that stops heartbeating within
  `child.livenessIntervalMs`. Once `complete_step` is durably accepted (WAL
  append + ACK, or a provisional `result` was observed) the turn is in its
  terminal phase and the abort window becomes the bounded
  `child.livenessIntervalMs + child.abortGraceMs`, so an ordinary
  post-completion teardown is not reaped as `ABORTED`; a child that exceeds the
  window still escalates `SIGTERM` → `SIGKILL` and resolves `ABORTED`. Every
  initiated abort records a machine-searchable reason — `watchdog_stale_heartbeat`,
  `watchdog_post_completion_timeout`, `control_plane_cancel`, `stream_loss`,
  `worker_drain`, `rpc_protocol_error`, `rpc_backlog_overflow`,
  `audit_backlog_overflow` — with heartbeat
  age, configured liveness/abort-grace, observed `complete_step`/provisional-result
  state, delivered signals, and final exit classification. The record is emitted
  as a structured `[harness:child-abort]` log line, counted as
  `control_plane_harness_child_aborts_total{reason}`, and appended to the
  terminal outcome message, so the durable evidence is not limited to the bare
  string `child killed by SIGKILL`.
- **Bounded fd-3 audit channel (HOR-612):** the child writes its audit frames
  (`event`/`tokenDelta`/`heartbeat`/`result`) through a bounded, non-blocking
  writer instead of a blocking `writeSync(3, …)`. A full pipe can therefore no
  longer block the child's event loop — which is also the loop the liveness
  heartbeat timer needs — and make the watchdog reap a healthy turn. Ephemeral
  `tokenDelta` frames are shed above a small budget and heartbeats are
  coalesced, so streamed model output cannot crowd out durable frames; the
  remaining backlog is bounded, and crossing the bound fails the channel closed:
  the child reports the exact queue depth, stall timings, and the frame kind that
  hit the full pipe over fd 4 (`auditBacklogOverflow`), then exits with
  `AUDIT_OVERFLOW_EXIT_CODE`. The supervisor records that evidence on the abort
  record and appends it to the terminal outcome message (`audit_queue=…`), so a
  stalled audit channel is attributable from the durable artifact alone. A
  heartbeat frame also carries the last-known audit-queue snapshot, so even a
  plain stale-heartbeat abort records the channel state it observed.
- **Open design question (recorded, not resolved):** whether a durably accepted
  `complete_step` should itself become the terminal turn boundary. HOR-551 keeps
  the clean-exit requirement and the completion/abort semantics unchanged;
  adopting that model needs separate explicit architecture approval.

## Internals (`src/`)

- `main.ts` — entry point: boot config + probes + supervisor + SIGTERM/SIGINT drain.
- `work-client.ts` — mTLS gRPC HTTP/2 transport + bidi `Work` stream + `Hello`/`Welcome`.
- `worker-state.ts` — protocol state machine + single-credit invariant.
- `supervisor.ts` — connect/turn loop, reconnect+backoff, heartbeat, outbox/replay,
  + gateway/model RPC dispatch (fd 4/fd 5 → tool/inference gateways).
- `event-outbox.ts` — per-turn outbox + WAL (fsync per event/ack; crash recovery).
- `child-process.ts` — spawn the child via the launcher + IPC (fd 0/3/4/5) + exit classification.
- `child.ts` — the pi child: `AgentSession` + custom `streamSimple` provider +
  gateway tool stubs + pi-event → `TurnEvent` mapping.
- `audit-channel.ts` — the bounded, non-blocking fd-3 child→supervisor audit
  writer (sheds ephemeral frames, bounds the durable backlog, reports evidence).
- `child-rpc.ts` — child→supervisor RPC demux (model/tool requests over fd 4/fd 5).
- `gateway-client.ts` — supervisor→gateway Connect client (tool discovery/invocation plus streaming artifacts).
- `artifact-files.ts` — verified input materialization + fd-bound secure publication.
- `model-bridge.ts` — supervisor→inference-gateway HTTP/2 mTLS SSE bridge.
- `openai-stream.ts` — OpenAI SSE → pi `AssistantMessageEvent` (child-owned model semantics).
- `ipc.ts` — framed discriminated-union IPC for fd 0/3/4/5 + runtime validation.
- `launcher.ts` — the `setpriv` privilege-dropping launcher (equal UID/GID, cleared groups, full cap-drop, `no_new_privs`, umask `0077`).
- `sandbox.ts` — canonical paths + ownership/mode/cwd validation.
- `tls-key.ts` — observe-only contained AtomicWriter-chain, protected-ancestor, and root-owned regular exact-`0440` resolved-key startup/readiness invariant.
- `storage-health.ts` — fail-closed runtime workspace fsync/rename/unlink health transaction.
- `config.ts` — infra-only boot config loader/validator.
- `probes.ts` — `/healthz` + `/readyz`.

## Image (`Dockerfile`)

`node:24-bookworm-slim`, multi-stage (`tsc` → `dist/`). The image defaults to
non-root (`65532`), but the AgentPool pod security context (HOR-245) runs the
**supervisor as root (UID/GID 0)** — required to read the cert-manager CSI
driver's root-owned exact-`0440` resolved tls.key target, `chown` per-session sandbox entries
under the root-owned `0711` mount root, and `setpriv`-drop to the session UID/GID.
The rendered container retains runtime-default capabilities and explicitly adds
`SETUID`/`SETGID`; it does not set `fsGroup` and does not have only those two
capabilities. PSS `restricted` forbids this boundary, hence `baseline`. The
session child has cleared groups/capabilities, `no_new_privs`, and umask `0077`,
and cannot read the key. Bakes pi + the supervisor + the child entry + the SDK
runtime. `setpriv` (util-linux) is present. Mounts at runtime (HOR-245): config, `/pi` overlay,
`/data/sandboxes` (PVC), TLS certs, the WAL emptyDir. No inbound RPC port — the
worker dials the control-plane. The root/runtime-default capability boundary and
explicit `SETUID`/`SETGID` additions are rendered by the pod security context,
not the image.

## Develop + test

```bash
make proto-tools          # buf + protoc plugins
make proto                # generate Go (internal/harnessrpc) + TS (src/gen)
make harness-build        # tsc -> dist
make harness-test         # vitest (unit + router-transport integration)
make harness-lint         # tsc --noEmit
make harness-image        # build the worker image
make harness-isolation-test   # privileged Linux: fresh ext4+XFS + production isolation
make harness-isolation-negative-test # prove an intentional cross-session access break is detected
```

Generated `src/gen/` + `internal/harnessrpc/` are committed; `make proto-check`
(CI) guards freshness. Tests: TS unit + router-transport integration (mock CP)
for the protocol/supervisor/outbox/child-process; the required, non-skippable
Linux-container isolation gate freshly formats and mounts real ext4 and XFS,
then runs the production `setpriv` probe for equal UID/GID, cleared groups,
capability drop, `no_new_privs`, `0077` umask, exact modes, sibling/key `EACCES`,
and TLS-key drift refusal in the selected harness CI job and root `make test`
matrix. `make harness-test` also runs the native Go mTLS test server (real TS-client ↔ Go-server wire), and
the isolation container's second phase proves sequential process-state isolation
and PVC-only restoration. E2E (real turn) is HOR-249/HOR-245 integration.

## Sequencing

HOR-381 is the prerequisite for HOR-245 (pool/operator/pod assembly + sandbox
provisioning + security context) and HOR-249 (dispatch + the `Work` server).
Both consume the `Work` contract this ticket defines. See the HOR-381 plan note
(Obsidian) + Linear HOR-381/245/249.

## Git workflow

Branches/commits/PRs carry the Linear ticket ID (e.g. `HOR-381`). See
`AGENTS.md`. Only the user approves/merges to `master`.
