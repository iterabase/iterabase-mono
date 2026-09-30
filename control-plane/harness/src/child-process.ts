// The per-turn Child (HOR-381): a real pi process spawned via the setpriv
// launcher under the session UID/GID. The supervisor talks to it over a
// dedicated duplex IPC channel of inherited fds:
//   - fd 0 (stdin)  : supervisor → child  (framed SupervisorFrame: assignment, abort)
//   - fd 3          : child → supervisor (framed ChildFrame: event, tokenDelta,
//                     heartbeat, result). Written by the child through the
//                     HOR-612 bounded, non-blocking audit writer: a full pipe
//                     sheds ephemeral frames and fails the channel closed with
//                     recorded evidence instead of blocking the child's event
//                     loop (and therefore its liveness heartbeat).
// stdout (fd 1) + stderr (fd 2) are piped separately and drained as tagged
// logs — they are NOT the protocol channel (approved trust boundary: length-
// prefixed JSON over a TS discriminated union with runtime validation, in
// ipc.ts). A stray/spoofed byte sequence cannot form a valid frame.
//
// Liveness: the child emits a heartbeat frame every livenessIntervalMs/2; if
// the supervisor sees no heartbeat within livenessIntervalMs it aborts —
// graceful SIGTERM, then SIGKILL after abortGraceMs (bounded escalation).
// Terminal-phase exception (HOR-551): once `complete_step` is durably accepted
// (supervisor WAL append + ack) or a provisional result was observed, the
// no-heartbeat window becomes the bounded livenessIntervalMs + abortGraceMs so
// an ordinary post-completion teardown is not reaped as ABORTED; a child that
// exceeds the window still escalates SIGTERM → SIGKILL and resolves ABORTED.
// Every initiated abort records a machine-searchable reason + bounded context
// (see ChildAbortRecord).
//
// Outcome classification: a `result` frame is PROVISIONAL. Success resolves
// only after a clean process exit (code 0) with an explicit valid result; an
// absent result is FAILED. A non-zero exit overrides a provisional COMPLETED
// (successful message + failed cleanup = FAILED). A signal exit during abort is
// ABORTED.
//
// The launch is injectable so the IPC + heartbeat + abort logic is unit-
// testable on macOS (setpriv is Linux-only).

import { type ChildProcess } from "node:child_process";
import type { Readable, Writable } from "node:stream";
import { fromJson, type JsonValue } from "@bufbuild/protobuf";
import { TurnEventSchema, Outcome, type AssignTurn } from "./gen/iterabase/harness/v1/harness_pb.js";
import { launchChild, type LaunchOptions } from "./launcher.js";
import type { Child, ChildAbortReason, ChildAbortRecord, ChildEvent, ChildResult, ChildFactory, ChildRpcRequest } from "./supervisor.js";
import type { HarnessConfig } from "./config.js";
import type { SandboxPaths } from "./sandbox.js";
import { AsyncQueue } from "./async-queue.js";
import { FrameReader, encodeFrame, parseChildFrame, parseChildRpcFrame, writeFrame, AUDIT_OVERFLOW_EXIT_CODE, type AuditChannelEvidence, type ChildRpcFrame } from "./ipc.js";

export type LaunchFn = (opts: LaunchOptions) => ChildProcess;

/** stdio for the child: fd0 control (sup→child), fd1 stdout, fd2 stderr, fd3 audit (child→sup), fd4 rpc requests (child→sup), fd5 rpc responses (sup→child). */
const CHILD_STDIO = ["pipe", "pipe", "pipe", "pipe", "pipe", "pipe"] as const;

/**
 * Build a ChildFactory that spawns the child entry `script` via the launcher.
 * `launch` defaults to launchChild (setpriv); tests inject a plain-node spawn.
 */
export function createChildFactory(cfg: HarnessConfig, script: string, launch: LaunchFn = launchChild): ChildFactory {
  const livenessMs = cfg.child.livenessIntervalMs;
  const graceMs = cfg.child.abortGraceMs;
  return (assignment: AssignTurn, sandbox: SandboxPaths, cwd: string): Child => {
    const sb = assignment.sandbox;
    if (!sb) throw new Error("AssignTurn missing sandbox");
    const proc = launch({
      script,
      uid: sb.uid,
      gid: sb.gid,
      sandboxRoot: sandbox.root,
      workingDir: sb.workingDir || "home",
      stdio: [...CHILD_STDIO],
      env: {
        HARNESS_SANDBOX_ROOT: sandbox.root,
        HARNESS_SESSION_DIR: sandbox.session,
        HARNESS_WORKING_DIR: cwd,
        HARNESS_PI_DIRS: cfg.piDirs.join(":"),
        HARNESS_MODEL_MAX_ATTEMPTS: String(cfg.modelRetry.maxAttempts),
        HARNESS_LIVENESS_INTERVAL_MS: String(livenessMs),
        // ModelRuntime's allowModelNetwork option controls only create-time
        // refresh. PI_OFFLINE also makes later extension/API refreshes default
        // to allowNetwork=false, preserving the credentialless IPC-only path.
        PI_OFFLINE: "1",
        HOME: sandbox.home,
        TMPDIR: sandbox.tmp,
      },
    });

    const events = new AsyncQueue<ChildEvent>();
    let resultResolved = false;
    let provisional: ChildResult | null = null; // PROVISIONAL until clean exit
    let resolveResult!: (r: ChildResult) => void;
    const result = new Promise<ChildResult>((r) => (resolveResult = r));
    const settle = (r: ChildResult) => {
      if (resultResolved) return;
      resultResolved = true;
      resolveResult(r);
    };

    // Supervisor → child control channel (fd 0). The assignment is the first frame.
    const control = proc.stdin;
    if (control) writeFrame(control, { type: "assignment", assignment: assignmentToJson(assignment) });

    // Child → supervisor RPC channel (fd 4): model/tool/cancel requests (HOR-395).
    const rpcRequests = new AsyncQueue<ChildRpcRequest>();
    const stdio = proc.stdio as unknown as (Readable | Writable | null)[];
    const rpcReqStream = stdio[4];
    if (rpcReqStream) {
      const rpcReader = new FrameReader(
        (raw) => {
          const f = parseChildRpcFrame(raw);
          if (!f) {
            // A malformed RPC request frame is stream corruption, not a
            // droppable audit frame. The child is now waiting on a response
            // that will never come, so terminate the turn fail-closed (HOR-395:
            // runtime validation must be bounded fail-closed, not a silent drop).
            failWithAbort("rpc_protocol_error", "invalid child RPC frame on fd 4");
            return;
          }
          if (f.type === "stepCompletion") stepCompletionObserved = true;
          rpcRequests.push(f as ChildRpcRequest);
        },
        // Strict mode for fd 4 (HOR-395 bounded fail-closed): framing/JSON/
        // truncation errors are NOT silently dropped here, because a dropped
        // request frame would leave the child waiting forever on a response
        // that never comes while its heartbeat stays healthy. Report every
        // such error to the turn failure + abort path.
        (reason) => {
          failWithAbort("rpc_protocol_error", `invalid child RPC frame on fd 4: ${reason}`);
        },
      );
      (rpcReqStream as Readable).on("data", (chunk: Buffer) => rpcReader.feed(chunk));
      (rpcReqStream as Readable).on("end", () => rpcReader.end());
      (rpcReqStream as Readable).on("error", () => rpcReader.end());
    }
    // Supervisor → child RPC channel (fd 5): framed responses (HOR-395).
    // Backpressure: rpcSend returns the value of `Writable.write()` (false →
    // the pipe buffer is full). Producers (the model bridge) must pause their
    // upstream reader and resume on `drain` via rpcOnDrain, so a slow child
    // cannot grow an unbounded queue in supervisor memory (HOR-395 bounded
    // buffering/backpressure).
    //
    // Hard aggregate response-queue bound (HOR-395): the model-chunk path
    // self-pauses on `false`, but the RPC dispatcher's control/terminal
    // responses (duplicate-id, overflow, toolResult, modelEnd) used to ignore
    // the boolean and `continue`, so a fast/compromised child that stops
    // reading fd 5 could send unlimited requests and make Node queue an
    // unbounded number of response frames despite the in-flight cap. We count
    // every write that returns `false` (a frame buffered in supervisor memory
    // beyond the pipe's high-water mark) and reset the count on `drain`; once
    // the backlog exceeds the cap, the turn fails closed (FAILED + abort) —
    // limiting active upstream calls alone does not bound supervisor memory.
    const rpcRespStream = stdio[5];
    let rpcBacklog = 0; // frames written while fd 5 is backpressured, not yet drained
    let rpcOverflowed = false;
    const MAX_RPC_BACKLOG = 64; // hard aggregate response-queue bound (HOR-395)
    const rpcSend = (frame: unknown): boolean => {
      if (rpcOverflowed) return false; // already failing closed — drop further writes
      try {
        if (rpcRespStream) {
          const ok = writeFrame(rpcRespStream as Writable, frame);
          if (!ok) {
            rpcBacklog += 1;
            if (rpcBacklog >= MAX_RPC_BACKLOG) {
              // fd 5 is not draining — bound supervisor memory by failing the
              // turn closed instead of queueing more response frames.
              rpcOverflowed = true;
              failWithAbort("rpc_backlog_overflow", "fd 5 response backlog overflow (child not draining)");
            }
          }
          return ok;
        }
      } catch {
        /* fd closed (child gone) — the exit handler classifies the outcome */
      }
      return true;
    };
    // Shared drain listener: when fd 5 drains, the backpressured backlog has
    // been flushed (the buffer dropped below the high-water mark). Swallow
    // async write errors (e.g. EPIPE when the child exits) — the exit handler
    // classifies the outcome; fd 5 is best-effort once the turn is settling.
    if (rpcRespStream) {
      (rpcRespStream as Writable).on("drain", () => { rpcBacklog = 0; });
      (rpcRespStream as Writable).on("error", () => { /* child gone — exit handler classifies */ });
    }

    const rpcOnDrain = (listener: () => void): (() => void) => {
      const s = rpcRespStream as Writable | null;
      if (!s) return () => {};
      const onDrain = (): void => listener();
      s.on("drain", onDrain);
      return () => s.off("drain", onDrain);
    };

    // Child → supervisor framed channel (fd 3).
    const frameStream = proc.stdio[3];
    let lastHeartbeat = Date.now();
    let watchdog: ReturnType<typeof setInterval> | null = null;
    let killTimer: ReturnType<typeof setTimeout> | null = null;
    let exited = false;
    let stepCompletionObserved = false;
    let stepCompletionDurable = false;
    // HOR-612: last child-reported fd-3 audit-channel state (heartbeat snapshot
    // while healthy, overflow notice when the child failed closed). Carried by
    // every abort record so a stalled audit channel is attributable from the
    // durable outcome alone.
    let auditChannelEvidence: AuditChannelEvidence | null = null;
    let abortRecord: ChildAbortRecord | null = null;

    const startWatchdog = (): void => {
      if (watchdog) return;
      watchdog = setInterval(() => {
        if (resultResolved || exited) return;
        const silentMs = Date.now() - lastHeartbeat;
        if (silentMs <= livenessMs) return;
        // OPEN DESIGN QUESTION (HOR-551, recorded but deliberately not
        // resolved): whether a durably accepted complete_step should itself
        // become the terminal turn boundary. Until that is decided by a
        // separate architecture approval, the clean-exit contract stands and
        // the terminal phase only gets a bounded post-completion window so an
        // ordinary post-completion teardown is not reaped as ABORTED.
        const terminalPhase = stepCompletionDurable || provisional !== null;
        if (terminalPhase && silentMs <= livenessMs + graceMs) return;
        forceAbort(terminalPhase ? "watchdog_post_completion_timeout" : "watchdog_stale_heartbeat");
      }, Math.max(50, Math.floor(livenessMs / 2)));
      watchdog.unref?.();
    };
    // Allow a startup grace (one interval) before enforcing, so a slow pi boot
    // is not misclassified as stale.
    lastHeartbeat = Date.now() + livenessMs; // effectively grants one interval of grace
    startWatchdog();

    if (frameStream) {
      const reader = new FrameReader((raw) => {
        const frame = parseChildFrame(raw);
        if (!frame) return; // malformed/unknown — drop (framing prevents spoofed audit)
        if (frame.type === "heartbeat") {
          lastHeartbeat = Date.now();
          if (frame.auditQueue) auditChannelEvidence = frame.auditQueue;
          return;
        }
        if (frame.type === "tokenDelta") {
          events.push({ kind: "tokenDelta", contentIndex: frame.contentIndex, deltaType: frame.deltaType, delta: frame.delta });
          return;
        }
        if (frame.type === "event") {
          try {
            const te = fromJson(TurnEventSchema, frame.event as JsonValue);
            events.push({ kind: "event", payload: te.kind });
          } catch {
            /* undecodable event payload — drop (validated before outbox) */
          }
          return;
        }
        if (frame.type === "result") {
          provisional = { outcome: frame.outcome as Outcome, message: frame.message };
          // HOR-434: the child reports its terminal result last and then cleans
          // up its own per-turn IPC and exits cleanly (child.ts
          // `releasePerTurnIpc` + `process.exit`). The supervisor keeps its fd-5
          // write end open; it does not tear the channels down here.
          return;
        }
      });
      frameStream.on("data", (chunk: Buffer) => reader.feed(chunk));
      frameStream.on("end", () => reader.end());
      frameStream.on("error", () => reader.end());
    }

    // Drain stdout/stderr as tagged logs (never the protocol channel). A full
    // pipe would block the child; draining keeps it bounded.
    proc.stdout?.on("data", (d: Buffer) => console.error(`[child:stdout] ${d.toString("utf8").trimEnd()}`));
    proc.stderr?.on("data", (d: Buffer) => console.error(`[child:stderr] ${d.toString("utf8").trimEnd()}`));

    const sendSignal = (signal: "SIGTERM" | "SIGKILL"): void => {
      try {
        // Record only signals actually delivered (a dead child returns false).
        if (proc.kill(signal)) abortRecord?.signals.push({ signal, sentAtMs: Date.now() });
      } catch {
        /* already dead */
      }
    };

    /**
     * Begin bounded abort escalation (SIGTERM → SIGKILL after abortGraceMs) and
     * record bounded, machine-searchable diagnostics (HOR-551). The first
     * caller wins: later aborts keep the original reason. No payloads, prompts,
     * arguments, results, or credentials are recorded.
     */
    const forceAbort = (reason: ChildAbortReason, detail?: string): ChildAbortRecord | null => {
      if (abortRecord) return abortRecord;
      if (exited) return null; // already reaped — nothing to abort
      const now = Date.now();
      const terminalPhase = stepCompletionDurable || provisional !== null;
      abortRecord = {
        reason,
        ...(detail ? { detail: detail.slice(0, 200) } : {}),
        initiatedAtMs: now,
        sinceHeartbeatMs: now - lastHeartbeat,
        livenessIntervalMs: livenessMs,
        abortGraceMs: graceMs,
        effectiveWindowMs: terminalPhase ? livenessMs + graceMs : livenessMs,
        terminalPhase,
        stepCompletionObserved,
        stepCompletionDurable,
        provisionalResultObserved: provisional !== null,
        ...(auditChannelEvidence ? { auditChannel: auditChannelEvidence } : {}),
        signals: [],
      };
      logChildAbort("initiated", abortRecord);
      sendSignal("SIGTERM");
      // Bounded escalation: SIGKILL after the grace if still alive.
      killTimer = setTimeout(() => sendSignal("SIGKILL"), graceMs);
      killTimer.unref?.();
      return abortRecord;
    };

    /** Fail the turn closed on a bounded abort path: initiate the abort with its
     * machine-searchable reason + bounded detail, then settle FAILED with the
     * diagnostic suffix and record. Single-sites the `abort=<reason>` suffix and
     * the null-record (already exited) case across the bounded-failure paths. */
    const failWithAbort = (reason: ChildAbortReason, message: string): void => {
      const record = forceAbort(reason, message);
      settle({
        outcome: Outcome.FAILED,
        message: record ? `${message} (abort=${record.reason})` : message,
        ...(record ? { abort: record } : {}),
      });
    };

    proc.on("error", (err) => {
      events.close();
      rpcRequests.close();
      settle({ outcome: Outcome.FAILED, message: `spawn error: ${err.message}` });
    });
    proc.on("exit", (code, signal) => {
      exited = true;
      events.close();
      rpcRequests.close();
      if (killTimer) clearTimeout(killTimer);
      if (watchdog) clearInterval(watchdog);
      if (abortRecord) {
        // Final exit classification for the abort diagnostics (HOR-551).
        abortRecord.finalCode = code;
        abortRecord.finalSignal = signal;
        logChildAbort("exited", abortRecord);
      }
      if (resultResolved) return;
      if (signal || abortRecord) {
        // Aborted (SIGTERM/SIGKILL) or killed by a signal. The message carries
        // the bounded, machine-searchable abort context so operators do not
        // have to rely on the bare terminal string `child killed by SIGKILL`.
        settle({
          outcome: Outcome.ABORTED,
          message: abortMessage(signal, abortRecord),
          ...(abortRecord ? { abort: abortRecord } : {}),
        });
      } else if (code === 0 && provisional) {
        // Clean exit with an explicit valid result — resolve the provisional.
        settle(provisional);
      } else if (code === 0) {
        // Clean exit WITHOUT a result — spec: an absent result is FAILED.
        settle({ outcome: Outcome.FAILED, message: "child exited without a result" });
      } else {
        // Non-zero exit overrides a provisional COMPLETED (failed cleanup = FAILED).
        // HOR-612: the child's audit-overflow exit code is attributed by name so
        // a turn whose terminal result could never be delivered is not an
        // anonymous non-zero exit in the durable outcome.
        settle({
          outcome: Outcome.FAILED,
          message: code === AUDIT_OVERFLOW_EXIT_CODE ? `child exit ${code} (fd-3 audit backlog overflow)` : `child exit ${code}`,
        });
      }
    });

    return {
      abort: forceAbort,
      noteStepCompletionDurable: () => {
        // The supervisor WAL-appended (durably accepted) and acked the child's
        // complete_step report. The child is now in its terminal phase: the
        // liveness watchdog applies the bounded post-completion window (HOR-551).
        stepCompletionDurable = true;
      },
      noteAuditChannelEvidence: (evidence: AuditChannelEvidence) => {
        // HOR-612: latest child-reported fd-3 audit-channel state. Recorded on
        // the next abort (heartbeat snapshots keep it fresh while the channel
        // is healthy; the overflow notice pins it when the child fails closed).
        auditChannelEvidence = evidence;
      },
      events,
      rpcRequests,
      rpcSend,
      rpcOnDrain,
      result,
    };
  };
}

/** Structured, bounded abort diagnostics line (HOR-551): a machine-searchable
 * JSON record with timing + lifecycle state only — no payloads, prompts, model
 * bodies, arguments, results, or credentials. */
function logChildAbort(phase: "initiated" | "exited", record: ChildAbortRecord): void {
  console.error(`[harness:child-abort] ${JSON.stringify({ phase, ...record })}`);
}

/** Compact machine-searchable abort context appended to the terminal outcome
 * message so the durable evidence does not reduce to the bare string
 * `child killed by SIGKILL` (HOR-551 acceptance). */
function abortMessage(signal: NodeJS.Signals | null, record: ChildAbortRecord | null): string {
  const cause = signal ? `child killed by ${signal}` : "aborted";
  if (!record) return cause;
  const completeStep = record.stepCompletionDurable ? "durable" : record.stepCompletionObserved ? "observed" : "none";
  const signals = record.signals.map((s) => s.signal).join("+") || "none";
  const detail = record.detail ? `; detail=${record.detail}` : "";
  const audit = record.auditChannel ? auditChannelSuffix(record.auditChannel) : "";
  return `${cause} [abort=${record.reason}; since_heartbeat_ms=${record.sinceHeartbeatMs}; liveness_ms=${record.livenessIntervalMs}; abort_grace_ms=${record.abortGraceMs}; terminal_phase=${record.terminalPhase}; complete_step=${completeStep}; provisional_result=${record.provisionalResultObserved}; signals=${signals}${audit}${detail}]`;
}

/** Compact, machine-searchable HOR-612 fd-3 audit-channel evidence suffix. */
function auditChannelSuffix(e: AuditChannelEvidence): string {
  const blocked = e.lastBlockedFrameKind ?? "none";
  return `; audit_queue=${e.queuedFrames}f/${e.queuedBytes}b; audit_since_write_ms=${e.sinceLastWriteMs}; audit_stalled_ms=${e.stalledMs}; audit_last_blocked=${blocked}; audit_dropped_token_deltas=${e.droppedTokenDeltas}; audit_coalesced_heartbeats=${e.coalescedHeartbeats}; audit_overflowed=${e.overflowed}`;
}

/** Serialize an AssignTurn to JSON for the child (the child reconstructs it). */
function assignmentToJson(at: AssignTurn): unknown {
  return {
    turnId: at.turnId,
    sessionId: at.sessionId,
    sandbox: at.sandbox ? { sandboxId: at.sandbox.sandboxId, uid: at.sandbox.uid, gid: at.sandbox.gid, workingDir: at.sandbox.workingDir } : null,
    persona: at.persona,
    model: at.model ? { id: at.model.id, api: at.model.api, contextWindow: at.model.contextWindow, maxOutputTokens: at.model.maxOutputTokens, thinkingLevel: at.model.thinkingLevel } : null,
    workspaceTools: at.workspaceTools,
    scopeIdentityId: at.scopeIdentityId,
    runId: at.runId,
    workItemId: at.workItemId,
    nodeExecutionId: at.nodeExecutionId,
    nodeKey: at.nodeKey,
    contextJson: at.contextJson,
    completionOutcomes: at.completionOutcomes,
    completionOutputSchemaJson: at.completionOutputSchemaJson,
    skills: at.skills.map((skill) => ({ name: skill.name, version: skill.version, digest: skill.digest })),
    artifactInputs: at.materializations.flatMap((materialization) => materialization.ref ? [{
      artifactId: materialization.ref.artifactId,
      mimeType: materialization.ref.mimeType,
      sizeBytes: materialization.ref.sizeBytes.toString(),
      digest: materialization.ref.digest,
    }] : []),
    message: at.message,
    images: at.images.map((img) => ({ data: Buffer.from(img.data).toString("base64"), mimeType: img.mimeType })),
  };
}

// Re-exported so callers that build raw frames (tests) can encode.
export { encodeFrame };
