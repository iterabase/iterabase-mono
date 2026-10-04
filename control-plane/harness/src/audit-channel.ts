// The child→supervisor fd-3 audit channel writer (HOR-612).
//
// fd 3 carries the child's audit frames (`event`, `tokenDelta`, `heartbeat`,
// `result`). The child used to write them with a blocking `writeSync(3, …)`:
// once the supervisor stopped draining the pipe, that write blocked the child's
// event loop — the same loop the liveness heartbeat timer needs — so a full
// pipe could stop the heartbeat and make the watchdog reap a *healthy* turn as
// `watchdog_stale_heartbeat` (observed under the pi 0.87.1 bump; the higher
// frame volume exposed it).
//
// This writer never blocks the child's event loop. Frames are queued in the
// child and flushed through a non-blocking fd-3 stream (`net.Socket`, the same
// event-driven uv_pipe pattern the fd-5 read path uses). Queueing is bounded
// and fail-closed, mirroring the supervisor's fd-5 `MAX_RPC_BACKLOG` decision
// (HOR-395) for the opposite direction:
//
//   1. ephemeral frames are shed first — `tokenDelta` frames are dropped
//      oldest-first above a small budget and `heartbeat` frames are coalesced —
//      so a burst of streamed model output can never crowd out the durable
//      `event`/`result` frames or the liveness heartbeat;
//   2. the remaining (durable) backlog is bounded by bytes and frames; crossing
//      the bound closes the channel and reports a bounded evidence snapshot
//      through `onOverflow` instead of growing child memory without limit.
//
// `flush()` is the terminal-path helper: the HOR-434 clean exit writes its
// `result` last and then exits, so the entrypoint must wait — bounded — until
// every queued frame has been handed to the kernel (a synchronous `writeSync`
// used to guarantee that implicitly).

import { Socket } from "node:net";
import { encodeFrame, type AuditChannelEvidence, type AuditFrameKind } from "./ipc.js";

/** Hard bound on the child-side durable fd-3 backlog (bytes). Deliberately
 * larger than `MAX_FRAME_BYTES` (16 MiB) so one legitimate oversized durable
 * frame cannot trip the bound by itself; only an accumulating backlog can. */
export const AUDIT_MAX_QUEUED_BYTES = 32 * 1024 * 1024;
/** Hard bound on the number of queued fd-3 frames (durable frames only, since
 * token deltas above the ephemeral budget and duplicate heartbeats are shed). */
export const AUDIT_MAX_QUEUED_FRAMES = 4_096;
/** Soft budget for queued ephemeral token deltas; above it they are dropped
 * oldest-first (they are best-effort, non-sequenced, non-ACKed by contract). */
export const AUDIT_MAX_EPHEMERAL_BYTES = 1 * 1024 * 1024;
/** Bounded wait for the terminal fd-3 flush, comfortably inside the default
 * liveness window so a stalled pipe cannot turn a reported result into a
 * watchdog reap (the supervisor classifies the absent result instead). */
export const AUDIT_FLUSH_TIMEOUT_MS = 2_000;

/** The minimal writable sink the channel needs: a `net.Socket` on fd 3 in
 * production, a deterministic fake in unit tests. */
export interface AuditSink {
  write(buf: Buffer, cb: (err?: Error | null) => void): boolean;
  on(event: string, listener: (...args: never[]) => void): unknown;
}

export interface AuditChannelOptions {
  /** Hard backlog bound overrides (tests); production uses the constants above. */
  maxQueuedBytes?: number;
  maxQueuedFrames?: number;
  maxEphemeralBytes?: number;
  /** Called exactly once when the hard bound is exceeded and the channel closes. */
  onOverflow?: (evidence: AuditChannelEvidence) => void;
}

function positiveEnvInt(env: NodeJS.ProcessEnv, name: string): number | undefined {
  const raw = env[name];
  if (raw === undefined || raw === "") return undefined;
  const n = Number(raw);
  return Number.isInteger(n) && n > 0 ? n : undefined;
}

/**
 * Bounded-writer seams (`HARNESS_AUDIT_BACKLOG_BYTES`,
 * `HARNESS_AUDIT_BACKLOG_FRAMES`, `HARNESS_AUDIT_EPHEMERAL_BYTES`). The
 * production supervisor does not set them, so the exported constants above
 * apply; tests (and a deliberate tuning change) can override them per child.
 */
export function auditChannelOptionsFromEnv(env: NodeJS.ProcessEnv = process.env): AuditChannelOptions {
  const options: AuditChannelOptions = {};
  const bytes = positiveEnvInt(env, "HARNESS_AUDIT_BACKLOG_BYTES");
  if (bytes !== undefined) options.maxQueuedBytes = bytes;
  const frames = positiveEnvInt(env, "HARNESS_AUDIT_BACKLOG_FRAMES");
  if (frames !== undefined) options.maxQueuedFrames = frames;
  const ephemeral = positiveEnvInt(env, "HARNESS_AUDIT_EPHEMERAL_BYTES");
  if (ephemeral !== undefined) options.maxEphemeralBytes = ephemeral;
  return options;
}

interface QueuedAuditFrame {
  buf: Buffer;
  kind: AuditFrameKind;
  /** Heartbeat frames are coalesced: at most one is outstanding at a time. */
  heartbeat: boolean;
}

/**
 * Bounded, non-blocking writer for the fd-3 audit channel. `write()` never
 * throws and never blocks; it accepts (or sheds) the frame and returns whether
 * the frame was accepted, so the caller can decide nothing further.
 */
export class AuditChannel {
  private readonly queue: QueuedAuditFrame[] = [];
  private queuedBytes = 0;
  private blocked = false;
  private overflowed = false;
  private inflightWrites = 0;
  private lastWriteOkAtMs = Date.now();
  private blockedSinceMs: number | null = null;
  private lastBlockedFrameKind: AuditFrameKind | undefined;
  private heartbeatOutstanding = false;
  private droppedTokenDeltas = 0;
  private coalescedHeartbeats = 0;
  private flushWaiters: Array<() => void> = [];
  private readonly maxQueuedBytes: number;
  private readonly maxQueuedFrames: number;
  private readonly maxEphemeralBytes: number;
  private readonly onOverflowCallback?: (evidence: AuditChannelEvidence) => void;

  constructor(
    private readonly sink: AuditSink,
    options: AuditChannelOptions = {},
  ) {
    this.maxQueuedBytes = options.maxQueuedBytes ?? AUDIT_MAX_QUEUED_BYTES;
    this.maxQueuedFrames = options.maxQueuedFrames ?? AUDIT_MAX_QUEUED_FRAMES;
    this.maxEphemeralBytes = options.maxEphemeralBytes ?? AUDIT_MAX_EPHEMERAL_BYTES;
    this.onOverflowCallback = options.onOverflow;
    // A write error (EPIPE — the supervisor went away) is not this writer's
    // problem to classify: the entrypoint's exit path owns the outcome, and the
    // pending write callbacks still fire so `flush()` stays bounded.
    this.sink.on("drain", () => this.onDrain());
    this.sink.on("error", () => {});
  }

  /** Create the production channel over the child's inherited fd 3. */
  static fromFd(fd: number, options: AuditChannelOptions = {}): AuditChannel {
    const socket = new Socket({ fd, readable: false, writable: true });
    return new AuditChannel(socket as unknown as AuditSink, options);
  }

  /**
   * Queue one framed audit frame. Returns false when this frame did not reach
   * the queue — a coalesced heartbeat, an ephemeral token delta shed by the
   * trim, or a channel that already failed closed — never because the caller
   * had to wait.
   */
  write(kind: AuditFrameKind, frame: unknown): boolean {
    if (this.overflowed) return false;
    if (kind === "heartbeat" && this.heartbeatOutstanding) {
      this.coalescedHeartbeats += 1;
      return false;
    }
    let buf: Buffer;
    try {
      buf = encodeFrame(frame);
    } catch {
      return false; // unframeable payload — never block the audit path on it
    }
    // A single ephemeral frame larger than the whole ephemeral budget is shed
    // outright; it could never fit without starving durable frames.
    if (kind === "tokenDelta" && buf.length > this.maxEphemeralBytes) {
      this.droppedTokenDeltas += 1;
      return false;
    }
    const queued: QueuedAuditFrame = { buf, kind, heartbeat: kind === "heartbeat" };
    this.queue.push(queued);
    this.queuedBytes += buf.length;
    if (kind === "heartbeat") this.heartbeatOutstanding = true;
    const shedPending = this.trimEphemeral(queued);
    this.pump();
    this.checkOverflow();
    return !shedPending && !this.overflowed;
  }

  /** Has the channel failed closed (hard backlog bound exceeded)? */
  get isOverflowed(): boolean {
    return this.overflowed;
  }

  /** The current bounded channel snapshot (queue depth, stall timing, shedding). */
  snapshot(): AuditChannelEvidence {
    const now = Date.now();
    const evidence: AuditChannelEvidence = {
      queuedFrames: this.queue.length,
      queuedBytes: this.queuedBytes,
      sinceLastWriteMs: Math.max(0, now - this.lastWriteOkAtMs),
      stalledMs: this.blockedSinceMs === null ? 0 : Math.max(0, now - this.blockedSinceMs),
      droppedTokenDeltas: this.droppedTokenDeltas,
      coalescedHeartbeats: this.coalescedHeartbeats,
      overflowed: this.overflowed,
    };
    if (this.lastBlockedFrameKind) evidence.lastBlockedFrameKind = this.lastBlockedFrameKind;
    return evidence;
  }

  /**
   * Resolve once every queued frame has been handed to the kernel (or the
   * bounded wait elapses). Returns whether the flush completed in time.
   */
  flush(timeoutMs: number = AUDIT_FLUSH_TIMEOUT_MS): Promise<boolean> {
    if (this.queue.length === 0 && this.inflightWrites === 0) return Promise.resolve(true);
    return new Promise<boolean>((resolve) => {
      let settled = false;
      const settle = (ok: boolean): void => {
        if (settled) return;
        settled = true;
        resolve(ok);
      };
      const timer = setTimeout(() => {
        this.flushWaiters = this.flushWaiters.filter((w) => w !== onFlush);
        settle(false);
      }, timeoutMs);
      const onFlush = (): void => {
        clearTimeout(timer);
        settle(true);
      };
      this.flushWaiters.push(onFlush);
    });
  }

  // ---- internals ----

  /** Drop oldest queued token deltas until the ephemeral budget holds. Returns
   * true when the just-queued `pending` frame was itself shed: a durable frame
   * already over the ephemeral budget keeps the queue over it, so the trim
   * clears the whole ephemeral tail including the frame that triggered it. */
  private trimEphemeral(pending: QueuedAuditFrame): boolean {
    if (this.queuedBytes <= this.maxEphemeralBytes) return false;
    let shedPending = false;
    for (let i = 0; i < this.queue.length && this.queuedBytes > this.maxEphemeralBytes; ) {
      const f = this.queue[i]!;
      if (f.kind !== "tokenDelta") {
        i += 1;
        continue;
      }
      this.queue.splice(i, 1);
      this.queuedBytes -= f.buf.length;
      this.droppedTokenDeltas += 1;
      if (f === pending) shedPending = true;
    }
    return shedPending;
  }

  /** Hand queued frames to the sink until it backpressures (or the queue drains). */
  private pump(): void {
    if (this.blocked || this.overflowed) return;
    while (this.queue.length > 0) {
      const frame = this.queue[0]!;
      let ok: boolean;
      try {
        this.inflightWrites += 1;
        ok = this.sink.write(frame.buf, (err) => this.onWriteDone(frame, err));
      } catch {
        // The sink is unusable (closed/foreign fd): the write callback will
        // never fire, so release its in-flight slot, drop the whole backlog and
        // stop pumping. The entrypoint's exit path classifies the turn and
        // `flush()` must never wait on a dead sink.
        this.inflightWrites -= 1;
        this.queue.length = 0;
        this.queuedBytes = 0;
        this.heartbeatOutstanding = false;
        this.resolveFlush();
        return;
      }
      this.queue.shift();
      this.queuedBytes -= frame.buf.length;
      if (!ok) {
        // Backpressure: the frame is buffered by the sink but its buffer is at
        // the high-water mark. Stop pumping until 'drain' so the sink's own
        // buffer stays bounded and our queue holds the (bounded) remainder.
        this.blocked = true;
        this.blockedSinceMs = Date.now();
        this.lastBlockedFrameKind = frame.kind;
        return;
      }
    }
    this.resolveFlush();
  }

  private onDrain(): void {
    this.blocked = false;
    this.blockedSinceMs = null;
    this.pump();
  }

  private onWriteDone(frame: QueuedAuditFrame, err?: Error | null): void {
    this.inflightWrites -= 1;
    if (!err) this.lastWriteOkAtMs = Date.now();
    if (frame.heartbeat) this.heartbeatOutstanding = false;
    this.resolveFlush();
  }

  private resolveFlush(): void {
    if (this.queue.length > 0 || this.inflightWrites > 0) return;
    const waiters = this.flushWaiters;
    this.flushWaiters = [];
    for (const w of waiters) w();
  }

  private checkOverflow(): void {
    if (this.overflowed) return;
    if (this.queuedBytes <= this.maxQueuedBytes && this.queue.length <= this.maxQueuedFrames) return;
    // Fail closed: close the channel, report the offending backlog once, then
    // release it. The child reports the overflow to the supervisor over fd 4
    // and exits non-zero (bounded fail-closed).
    this.overflowed = true;
    if (this.lastBlockedFrameKind === undefined) this.lastBlockedFrameKind = this.queue.at(-1)?.kind;
    const evidence = this.snapshot(); // queue depth at overflow, with overflowed=true
    this.queue.length = 0;
    this.queuedBytes = 0;
    this.onOverflowCallback?.(evidence);
  }
}
