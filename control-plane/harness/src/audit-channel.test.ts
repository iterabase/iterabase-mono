// HOR-612: the child→supervisor fd-3 audit channel must never block the child's
// event loop. This suite drives the bounded writer with a deterministic sink
// (the production fd-3 `net.Socket`, but synchronous and controllable here) and
// asserts the invariants the liveness channel depends on:
//   - a stalled reader never blocks `write()`;
//   - ephemeral frames are shed first, so durable `event`/`result` frames and
//     the heartbeat are never starved by streamed token deltas;
//   - the durable backlog is bounded, and exceeding it fails the channel closed
//     with the evidence an abort record needs;
//   - the terminal flush is bounded.

import { describe, expect, it } from "vitest";
import { AuditChannel, auditChannelOptionsFromEnv, AUDIT_MAX_EPHEMERAL_BYTES, type AuditSink } from "./audit-channel.js";
import type { AuditChannelEvidence } from "./ipc.js";

/** A controllable fd-3 sink: records the frames it was handed, and can stall
 * (buffer + backpressure) exactly like a full pipe with a non-draining reader. */
class FakeSink implements AuditSink {
  /** Every frame handed to the sink, in order: `{ type }`. */
  readonly frames: Array<{ type?: string }> = [];
  stalled = false;
  private readonly buffered: Array<(err?: Error | null) => void> = [];
  private readonly listeners = new Map<string, Array<(...args: never[]) => void>>();

  write(buf: Buffer, cb: (err?: Error | null) => void): boolean {
    this.frames.push(JSON.parse(buf.subarray(4).toString("utf8")) as { type?: string });
    if (this.stalled) {
      this.buffered.push(cb);
      return false;
    }
    cb(null);
    return true;
  }

  on(event: string, listener: (...args: never[]) => void): this {
    const list = this.listeners.get(event) ?? [];
    list.push(listener);
    this.listeners.set(event, list);
    return this;
  }

  /** The reader resumed: complete the buffered writes and emit `drain`. */
  drain(): void {
    this.stalled = false;
    const buffered = this.buffered.splice(0);
    for (const cb of buffered) cb(null);
    for (const l of this.listeners.get("drain") ?? []) l();
  }

  typesAfter(index: number): Array<string | undefined> {
    return this.frames.slice(index).map((f) => f.type);
  }
}

function durableEvent(size = 2_000): unknown {
  return { type: "event", event: { turnId: "t", sequence: "0", timestampMs: "0", harnessError: { error: { message: "x".repeat(size) } } } };
}
function tokenDelta(size = 500): unknown {
  return { type: "tokenDelta", contentIndex: 0, deltaType: "TEXT", delta: "x".repeat(size) };
}

describe("AuditChannel (HOR-612 bounded fd-3 writer)", () => {
  it("does not block the writer when fd 3 stops draining, and fails closed with bounded evidence", () => {
    const sink = new FakeSink();
    const overflows: AuditChannelEvidence[] = [];
    const channel = new AuditChannel(sink, {
      maxQueuedBytes: 100_000,
      maxQueuedFrames: 10_000,
      maxEphemeralBytes: 20_000,
      onOverflow: (e) => overflows.push(e),
    });
    sink.stalled = true; // the supervisor stops reading fd 3

    const started = performance.now();
    for (let i = 0; i < 500; i++) channel.write("event", durableEvent());
    const elapsed = performance.now() - started;

    // The writer never waits on the pipe; the whole burst is absorbed (or shed).
    expect(elapsed).toBeLessThan(1_000);
    expect(overflows).toHaveLength(1);
    const evidence = overflows[0]!;
    expect(evidence.overflowed).toBe(true);
    expect(evidence.queuedBytes).toBeGreaterThan(100_000); // depth at overflow, not after release
    expect(evidence.queuedFrames).toBeGreaterThan(0);
    expect(evidence.lastBlockedFrameKind).toBe("event");
    expect(evidence.stalledMs).toBeGreaterThanOrEqual(0);
    expect(channel.isOverflowed).toBe(true);
    // The channel is closed: further frames are refused, no second overflow.
    expect(channel.write("result", { type: "result", outcome: 1 })).toBe(false);
    expect(overflows).toHaveLength(1);
    const framesAfterOverflow = sink.frames.length;
    expect(channel.write("event", durableEvent())).toBe(false);
    expect(sink.frames.length).toBe(framesAfterOverflow);
  });

  it("sheds token deltas and coalesces heartbeats so the liveness frame is delivered by its next interval", () => {
    const sink = new FakeSink();
    const channel = new AuditChannel(sink, {
      maxQueuedBytes: 10_000_000,
      maxQueuedFrames: 100_000,
      maxEphemeralBytes: 2_000,
      onOverflow: () => {
        throw new Error("must not overflow: ephemeral frames are shed, not queued");
      },
    });
    sink.stalled = true;

    // A burst of streamed model output while the pipe is full.
    for (let i = 0; i < 100; i++) channel.write("tokenDelta", tokenDelta());
    channel.write("heartbeat", { type: "heartbeat" });
    channel.write("heartbeat", { type: "heartbeat" });
    channel.write("heartbeat", { type: "heartbeat" });

    // One durable event must also survive the burst, after the shed head.
    channel.write("event", durableEvent());

    const deliveredBefore = sink.frames.length;
    sink.drain(); // reader resumes

    const delivered = sink.typesAfter(deliveredBefore);
    // Only the ephemeral budget of token deltas is still queued; the heartbeat
    // and the durable event follow immediately instead of behind 50 KB of
    // streamed text that no longer matters.
    expect(delivered.filter((t) => t === "tokenDelta").length).toBeLessThanOrEqual(4);
    expect(delivered).toContain("heartbeat");
    expect(delivered).toContain("event");

    const snapshot = channel.snapshot();
    expect(snapshot.droppedTokenDeltas).toBeGreaterThan(90);
    expect(snapshot.coalescedHeartbeats).toBe(2); // 3 writes, 1 outstanding
    expect(snapshot.overflowed).toBe(false);
  });

  it("never drops durable frames while ephemeral frames are being shed", () => {
    const sink = new FakeSink();
    const channel = new AuditChannel(sink, { maxQueuedBytes: 10_000_000, maxQueuedFrames: 100_000, maxEphemeralBytes: 1_000 });
    sink.stalled = true;
    for (let i = 0; i < 20; i++) {
      channel.write("tokenDelta", tokenDelta());
      channel.write("event", durableEvent(1_000));
    }
    const before = sink.frames.length;
    sink.drain();
    const delivered = sink.typesAfter(before);
    // Every durable event is delivered, in order: only ephemeral deltas shed.
    expect(delivered.filter((t) => t === "event")).toHaveLength(20);
    expect(delivered.filter((t) => t === "tokenDelta").length).toBeLessThanOrEqual(2);
  });

  it("bounds the terminal flush while the pipe cannot drain, and completes when it can", async () => {
    const sink = new FakeSink();
    const channel = new AuditChannel(sink, { maxQueuedBytes: 10_000_000, maxQueuedFrames: 100_000, maxEphemeralBytes: 1_000 });
    sink.stalled = true;
    for (let i = 0; i < 10; i++) channel.write("event", durableEvent());
    await expect(channel.flush(25)).resolves.toBe(false); // bounded, never hangs the exit path
    sink.drain();
    await expect(channel.flush(250)).resolves.toBe(true);
  });

  it("reports the stall timing and the frame kind that hit the full pipe", async () => {
    const sink = new FakeSink();
    const channel = new AuditChannel(sink, { maxQueuedBytes: 10_000_000, maxQueuedFrames: 100_000, maxEphemeralBytes: 1_000 });
    sink.stalled = true;
    channel.write("heartbeat", { type: "heartbeat" });
    await new Promise((r) => setTimeout(r, 20));
    const snapshot = channel.snapshot();
    expect(snapshot.lastBlockedFrameKind).toBe("heartbeat");
    expect(snapshot.stalledMs).toBeGreaterThan(0);
    expect(snapshot.sinceLastWriteMs).toBeGreaterThan(0);
    expect(snapshot.queuedFrames).toBe(0); // the stalled frame lives in the sink's own buffer
  });

  it("reads the bounded-writer seams from the environment and ignores invalid values", () => {
    expect(auditChannelOptionsFromEnv({})).toEqual({});
    expect(
      auditChannelOptionsFromEnv({
        HARNESS_AUDIT_BACKLOG_BYTES: "4096",
        HARNESS_AUDIT_BACKLOG_FRAMES: "16",
        HARNESS_AUDIT_EPHEMERAL_BYTES: "512",
      }),
    ).toEqual({ maxQueuedBytes: 4_096, maxQueuedFrames: 16, maxEphemeralBytes: 512 });
    expect(
      auditChannelOptionsFromEnv({ HARNESS_AUDIT_BACKLOG_BYTES: "0", HARNESS_AUDIT_BACKLOG_FRAMES: "nope", HARNESS_AUDIT_EPHEMERAL_BYTES: "-5" }),
    ).toEqual({});
    expect(AUDIT_MAX_EPHEMERAL_BYTES).toBeGreaterThan(0);
  });
});
