import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync, chmodSync, mkdirSync, existsSync, lstatSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, basename } from "node:path";
import {
  Harness,
  WorkerMessageSchema,
  HelloSchema,
  ControlMessageSchema,
  WelcomeSchema,
  AssignTurnSchema,
  SandboxRefSchema,
  ModelConfigSchema,
  EventAckSchema,
  AssistantMessageSchema,
  SessionEndSchema,
  WorkspaceStatusSchema,
  AbortTurnSchema,
  Outcome,
  type AssignTurn,
  type WorkerMessage,
} from "./gen/iterabase/harness/v1/harness_pb.js";
import { Supervisor, type Child, type ChildEvent, type ChildResult, type TurnEventPayload } from "./supervisor.js";
import { createChildFactory } from "./child-process.js";
import { Probes } from "./probes.js";
import { EventOutbox } from "./event-outbox.js";
import type { HarnessConfig } from "./config.js";

// Minimal async queue for the FakeChild's event stream.
class Q<T> implements AsyncIterable<T> {
  private buf: T[] = [];
  private closed = false;
  private waiters: Array<(r: IteratorResult<T>) => void> = [];
  push(v: T): void {
    if (this.closed) return;
    const w = this.waiters.shift();
    if (w) w({ value: v, done: false });
    else this.buf.push(v);
  }
  close(): void {
    this.closed = true;
    for (const w of this.waiters) w({ value: undefined as never, done: true });
    this.waiters.length = 0;
  }
  [Symbol.asyncIterator](): AsyncIterator<T> {
    return {
      next: () => {
        if (this.buf.length) return Promise.resolve({ value: this.buf.shift() as T, done: false });
        if (this.closed) return Promise.resolve({ value: undefined as never, done: true });
        return new Promise((r) => this.waiters.push(r));
      },
    };
  }
}

/** A FakeChild: emits the given events, then resolves `result`. */
function fakeChild(events: ChildEvent[], outcome: Outcome, message?: string): Child {
  const q = new Q<ChildEvent>();
  for (const e of events) q.push(e);
  q.close();
  const rpcQ = new Q<import("./supervisor.js").ChildRpcRequest>();
  rpcQ.close(); // no RPC requests in these tests
  const result = Promise.resolve<ChildResult>({ outcome, message });
  return { abort: () => {}, noteStepCompletionDurable: () => {}, noteAuditChannelEvidence: () => {}, events: q, rpcRequests: rpcQ, rpcSend: () => {}, result };
}

const UID = process.getuid();
const GID = process.getgid();

function makeCfg(sandboxRoot: string, walDir: string): HarnessConfig {
  return {
    controlPlane: { url: "https://cp", serverName: "cp" },
    worker: { workerId: "pod-1", poolId: "pool-1" },
    tls: { cert: "", key: "", ca: "" },
    sandboxRoot,
    piDirs: [],
    toolGateway: { url: "https://localhost:8442", serverName: "tool-gateway" },
    inferenceGateway: { url: "https://localhost:8443", serverName: "inference-gateway" },
    walDir,
    probe: { port: 0 },
    transport: { http2PingIntervalMs: 30000, http2PingTimeoutMs: 10000 },
    reconnect: { initialBackoffMs: 1, maxBackoffMs: 2, resetAfterMs: 1000 },
    child: { livenessIntervalMs: 1000, abortGraceMs: 1000 },
    outbox: { bound: 4096 },
    modelRetry: { maxAttempts: 3 },
    tokenDelta: { sendBufferBytes: 1048576 },
  } as HarnessConfig;
}

describe("Supervisor turn loop", () => {
  let sandboxParent: string;
  let walDir: string;
  let sandboxId: string;
  let probes: Probes;

  beforeEach(() => {
    sandboxParent = mkdtempSync(join(tmpdir(), "harness-sup-"));
    walDir = mkdtempSync(join(tmpdir(), "harness-wal-"));
    sandboxId = "sess-a";
    // The supervisor provisions the per-session sandbox itself (HOR-245); tests
    // that need a pre-existing (e.g. mismatched) root create it in their body.
    probes = new Probes();
  });
  afterEach(() => {
    rmSync(sandboxParent, { recursive: true, force: true });
    rmSync(walDir, { recursive: true, force: true });
  });

  it("connects, runs a turn (child events + COMPLETED), ACKs, re-advertises a credit", async () => {
    const received: WorkerMessage[] = [];
    let assignTurnSent = false;

    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: {
              case: "welcome",
              value: create(WelcomeSchema, {
                protocolVersion: "1",
                fencingGeneration: 1n,
                heartbeatIntervalMs: 60000,
                leaseTimeoutMs: 120000,
              }),
            },
          });
          for await (const m of req) {
            received.push(m);
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn(sandboxId);
            } else if (m.kind.case === "turnEvent") {
              const te = m.kind.value;
              if (te.kind.case === "workerOutcome") {
                yield create(ControlMessageSchema, {
                  kind: {
                    case: "eventAck",
                    value: create(EventAckSchema, { turnId: te.turnId, throughSequence: te.sequence }),
                  },
                });
              }
            }
          }
        },
      });
    });

    let credits = 0;
    const credit2 = new Promise<void>((r) => {
      const orig = () => {
        credits += 1;
        if (credits === 2) r();
      };
      // patch: stash orig for the dep callback
      (globalThis as { __creditCb?: () => void }).__creditCb = orig;
    });
    const onCreditAdvertised = () => (globalThis as { __creditCb?: () => void }).__creditCb?.();

    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, {
        kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) },
      }),
      childFactory: () =>
        fakeChild(
          [
            {
              kind: "event",
              payload: {
                case: "assistantMessage",
                value: create(AssistantMessageSchema, { text: "classified: pricing" }),
              },
            },
          ],
          Outcome.COMPLETED,
        ),
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
      onCreditAdvertised,
    });

    const runP = sup.run();
    await credit2; // initial credit + post-turn credit
    await sup.drain();
    await runP;

    // The CP received: Hello(initial, via openWorkStream), Ready x2, the assistant message, the COMPLETED outcome, + heartbeats.
    const kinds = received.map((m) => m.kind.case);
    expect(kinds.filter((k) => k === "ready").length).toBe(2);
    expect(kinds).toContain("turnEvent");
    const outcomes = received
      .filter((m) => m.kind.case === "turnEvent" && m.kind.value.kind.case === "workerOutcome")
      .map((m) => m.kind!.value!.kind!.value as { outcome: Outcome });
    expect(outcomes[0]?.outcome).toBe(Outcome.COMPLETED);

    // HOR-245: the supervisor provisioned the per-session sandbox itself
    // (root + home/tmp/session/workspace, 0700, owned by the session UID/GID).
    for (const sub of ["home", "tmp", "session", "workspace"]) {
      const st = lstatSync(join(sandboxParent, sandboxId, sub));
      expect(st.isDirectory()).toBe(true);
      expect(st.mode & 0o777).toBe(0o700);
    }
  });

  it("does not duplicate Ready while a server-consumed assignment is in flight during a capacity observation", async () => {
    const received: WorkerMessage[] = [];
    let assignmentConsumed!: () => void;
    const consumed = new Promise<void>((resolve) => { assignmentConsumed = resolve; });
    let releaseAssignment!: () => void;
    const assignmentGate = new Promise<void>((resolve) => { releaseAssignment = resolve; });
    let assignmentSent = false;
    let secondReadyReceived!: () => void;
    const postTurnReady = new Promise<void>((resolve) => { secondReadyReceived = resolve; });

    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) } as never,
          });
          for await (const m of req) {
            received.push(m);
            if (m.kind.case === "ready" && !assignmentSent) {
              // Model dispatch's exact race window: the server has consumed the
              // one Ready credit, but AssignTurn is deliberately withheld.
              assignmentSent = true;
              assignmentConsumed();
              await assignmentGate;
              yield assignTurn(sandboxId);
            } else if (m.kind.case === "ready") {
              secondReadyReceived();
            } else if (m.kind.case === "turnEvent" && m.kind.value.kind.case === "workerOutcome") {
              yield create(ControlMessageSchema, {
                kind: {
                  case: "eventAck",
                  value: create(EventAckSchema, { turnId: m.kind.value.turnId, throughSequence: m.kind.value.sequence }),
                },
              });
            }
          }
        },
      });
    });

    const openStatus = create(WorkspaceStatusSchema, {
      freeBytes: 30n,
      capacityBytes: 100n,
      freeRatio: 0.30,
      warning: false,
      creditGated: false,
    });
    let credits = 0;
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, {
        kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) },
      }),
      childFactory: () => fakeChild([], Outcome.COMPLETED),
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
      workspaceStatus: openStatus,
      onCreditAdvertised: () => { credits += 1; },
    });

    const runP = sup.run();
    await consumed;
    sup.updateWorkspaceStatus(openStatus);
    await Promise.resolve();
    expect(credits).toBe(1);

    releaseAssignment();
    await postTurnReady;
    await sup.drain();
    await runP;

    expect(received.filter((m) => m.kind.case === "ready")).toHaveLength(2);
  }, 5_000);

  it("reaps the per-session sandbox on SessionEnd after a completed turn (HOR-245 cleanup)", async () => {
    let assignTurnSent = false;
    let sessionEnded = false;
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: {
              case: "welcome",
              value: create(WelcomeSchema, { fencingGeneration: 1n }) as never,
            } as never,
          });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn(sandboxId);
            } else if (m.kind.case === "turnEvent") {
              const te = m.kind.value;
              if (te.kind.case === "workerOutcome" && !sessionEnded) {
                sessionEnded = true;
                yield create(ControlMessageSchema, {
                  kind: { case: "eventAck", value: create(EventAckSchema, { turnId: te.turnId, throughSequence: te.sequence }) },
                });
                // After the final ACK the worker is idle; the CP reaps the
                // terminated session's sandbox (HOR-245 cleanup owner).
                yield create(ControlMessageSchema, {
                  kind: { case: "sessionEnd", value: create(SessionEndSchema, { sandboxId, uid: UID, gid: GID }) },
                });
              }
            }
          }
        },
      });
    });
    let credits = 0;
    const credit2 = new Promise<void>((r) => {
      (globalThis as { __creditCb?: () => void }).__creditCb = () => { credits += 1; if (credits === 2) r(); };
    });
    const onCreditAdvertised = () => (globalThis as { __creditCb?: () => void }).__creditCb?.();
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: () => fakeChild([{ kind: "event", payload: { case: "assistantMessage", value: create(AssistantMessageSchema, { text: "done" }) } }], Outcome.COMPLETED),
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
      onCreditAdvertised,
    });
    const runP = sup.run();
    await credit2; // initial credit + post-turn credit (idle after ACK)
    await new Promise((r) => setTimeout(r, 100)); // let SessionEnd process
    await sup.drain();
    await runP;
    // The sandbox the supervisor provisioned at AssignTurn is now reaped.
    expect(() => lstatSync(join(sandboxParent, sandboxId))).toThrow();
  }, 5_000);

  it("treats SessionEnd during an active turn as a protocol violation (fatal, no reap)", async () => {
    let assignTurnSent = false;
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) as never } as never,
          });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn(sandboxId);
              // Dispatch bug: SessionEnd arrives while the turn is still running.
              yield create(ControlMessageSchema, {
                kind: { case: "sessionEnd", value: create(SessionEndSchema, { sandboxId, uid: UID, gid: GID }) },
              });
            }
          }
        },
      });
    });
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: () => fakeChild([], Outcome.COMPLETED),
      probes,
      transport: () => transport,
      gatewayClient: { ...fakeGatewayClient(), discover: async () => new Promise(() => {}) /* never resolves: stay running */ },
      modelStream: fakeModelStream(),
    });
    const runP = sup.run();
    await runP; // fatal makes run() return
    // The sandbox was provisioned at AssignTurn but NOT reaped (SessionEnd was
    // rejected as a protocol violation during the active turn).
    expect(lstatSync(join(sandboxParent, sandboxId)).isDirectory()).toBe(true);
  }, 5_000);

  it("intersects workspace_tools with the pool maximum (ARCH-016 interim residual)", async () => {
    let assignTurnSent = false;
    let captured: AssignTurn | undefined;
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: {
              case: "welcome",
              value: create(WelcomeSchema, { fencingGeneration: 1n }) as never,
            } as never,
          });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn(sandboxId);
            } else if (m.kind.case === "turnEvent" && m.kind.value.kind.case === "workerOutcome") {
              yield create(ControlMessageSchema, {
                kind: {
                  case: "eventAck",
                  value: create(EventAckSchema, { turnId: m.kind.value.turnId, throughSequence: m.kind.value.sequence }),
                },
              });
            }
          }
        },
      });
    });

    // poolWorkspaceTools unset → deny-by-default; assignment requests true.
    const cfg = makeCfg(sandboxParent, walDir);
    const sup = new Supervisor({
      cfg,
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: (at) => { captured = at; return fakeChild([], Outcome.COMPLETED); },
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
      onCreditAdvertised: () => {},
    });
    const runP = sup.run();
    await new Promise<void>((r) => setTimeout(r, 150));
    await sup.drain();
    await runP;
    expect(captured?.workspaceTools).toBe(false); // widened request denied
  });

  it("emits FAILED when the sandbox root pre-exists with a wrong mode (never chowned)", async () => {
    // HOR-245: the provisioner never chowns an existing path. A pre-existing
    // mismatched root (wrong mode) is refused -> typed FAILED (not auto-fixed).
    const badRoot = join(sandboxParent, "sess-a");
    mkdirSync(badRoot, { mode: 0o755 });
    chmodSync(badRoot, 0o755);
    const received: WorkerMessage[] = [];
    let assignTurnSent = false;
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) } as never,
          });
          for await (const m of req) {
            received.push(m);
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn(sandboxId);
            } else if (m.kind.case === "turnEvent" && m.kind.value.kind.case === "workerOutcome") {
              yield create(ControlMessageSchema, {
                kind: {
                  case: "eventAck",
                  value: create(EventAckSchema, {
                    turnId: m.kind.value.turnId,
                    throughSequence: m.kind.value.sequence,
                  }),
                },
              });
            }
          }
        },
      });
    });

    let credits = 0;
    const credit2 = new Promise<void>((r) => {
      (globalThis as { __creditCb?: () => void }).__creditCb = () => {
        credits += 1;
        if (credits === 2) r();
      };
    });
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, {
        kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) },
      }),
      childFactory: () => fakeChild([], Outcome.COMPLETED), // never reached (sandbox invalid)
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
      onCreditAdvertised: () => (globalThis as { __creditCb?: () => void }).__creditCb?.(),
    });

    const runP = sup.run();
    await credit2;
    await sup.drain();
    await runP;

    const outcomes = received
      .filter((m) => m.kind.case === "turnEvent" && m.kind.value.kind.case === "workerOutcome")
      .map((m) => m.kind!.value!.kind!.value as { outcome: Outcome });
    expect(outcomes[0]?.outcome).toBe(Outcome.FAILED);
    // A harness error preceded the outcome.
    const hasHarnessError = received.some(
      (m) => m.kind.case === "turnEvent" && m.kind.value.kind.case === "harnessError",
    );
    expect(hasHarnessError).toBe(true);
  });

  it("HOR-551: does not reap a durable complete_step as ABORTED when the post-completion lifecycle crosses the 5s liveness window", async () => {
    // Production-like watchdog settings: the ticket explicitly rejects hiding
    // the boundary behind the existing 60s test configuration. The stub child
    // heartbeats once, durably reports complete_step (WAL append + ack), then
    // goes silent for 9s (> one 5s liveness interval and past the pre-fix 5-7.5s
    // abort tick) before emitting its terminal result and exiting cleanly.
    const cfg = makeCfg(sandboxParent, walDir);
    cfg.child = { livenessIntervalMs: 5_000, abortGraceMs: 10_000 };
    const stubScript = join(sandboxParent, "hor551-post-completion-child.cjs");
    writeFileSync(
      stubScript,
      `
const fs = require('fs');
const net = require('net');
function frame(fd, obj) {
  const j = Buffer.from(JSON.stringify(obj));
  const h = Buffer.alloc(4);
  h.writeUInt32BE(j.length, 0);
  fs.writeSync(fd, Buffer.concat([h, j]));
}
frame(3, { type: 'heartbeat' });
process.stdin.on('data', () => {});
let buf = Buffer.alloc(0);
// Mirror the production child: read fd 5 as a non-blocking uv_pipe so the
// later releasePerTurnIpc + process.exit(0) clean exit is not blocked by a
// pending libuv threadpool read (HOR-434).
const rpc = new net.Socket({ fd: 5, readable: true, writable: false });
rpc.on('data', (chunk) => {
  buf = Buffer.concat([buf, chunk]);
  while (buf.length >= 4) {
    const len = buf.readUInt32BE(0);
    if (buf.length < 4 + len) break;
    const msg = JSON.parse(buf.subarray(4, 4 + len).toString('utf8'));
    buf = buf.subarray(4 + len);
    if (msg.type === 'gatewayTools') {
      frame(4, { type: 'stepCompletion', requestId: 'c1', outcome: 'completed', summary: 'done', outputJson: '{}', artifactRefs: [] });
    } else if (msg.type === 'stepCompletionAck') {
      setTimeout(() => {
        frame(3, { type: 'result', outcome: 1 });
        try { rpc.removeAllListeners(); rpc.destroy(); } catch {}
        try { fs.closeSync(5); } catch {}
        process.stdin.removeAllListeners();
        try { process.stdin.destroy(); } catch {}
        process.exit(0);
      }, 9000);
    }
  }
});
`,
    );

    const received: WorkerMessage[] = [];
    let assignTurnSent = false;
    let stepCompletionAt = 0;
    let outcomeAt = 0;
    let outcomeSeen!: () => void;
    const outcomeReceived = new Promise<void>((resolve) => (outcomeSeen = resolve));
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: {
              case: "welcome",
              value: create(WelcomeSchema, { protocolVersion: "1", fencingGeneration: 1n, heartbeatIntervalMs: 60000, leaseTimeoutMs: 120000 }),
            },
          });
          for await (const m of req) {
            received.push(m);
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn(sandboxId, true);
            } else if (m.kind.case === "turnEvent") {
              const te = m.kind.value;
              if (te.kind.case === "stepCompletion") stepCompletionAt = Date.now();
              if (te.kind.case === "workerOutcome") {
                outcomeAt = Date.now();
                yield create(ControlMessageSchema, {
                  kind: { case: "eventAck", value: create(EventAckSchema, { turnId: te.turnId, throughSequence: te.sequence }) },
                });
                outcomeSeen();
              }
            }
          }
        },
      });
    });

    const sup = new Supervisor({
      cfg,
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: createChildFactory(cfg, stubScript, (opts) =>
        spawn(process.execPath, [opts.script], {
          cwd: join(opts.sandboxRoot, opts.workingDir),
          stdio: opts.stdio as never,
          env: { ...opts.env, PATH: process.env.PATH ?? "" },
        }),
      ),
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
    });

    const runP = sup.run();
    await outcomeReceived;
    await sup.drain();
    await runP;

    // complete_step was accepted and durably emitted (WAL append) before the
    // crossing, and the post-completion lifecycle really crossed one liveness
    // interval before the terminal result arrived.
    const stepEvent = received.find((m) => m.kind.case === "turnEvent" && m.kind.value.kind.case === "stepCompletion");
    expect(stepEvent).toBeDefined();
    expect(outcomeAt - stepCompletionAt).toBeGreaterThanOrEqual(5_000);
    const outcomes = received
      .filter((m) => m.kind.case === "turnEvent" && m.kind.value.kind.case === "workerOutcome")
      .map((m) => m.kind!.value!.kind!.value as { outcome: Outcome; message?: string });
    // Exactly one terminal outcome, and it is the COMPLETED one — the durable
    // complete_step is never later replaced by an ABORTED projection.
    expect(outcomes).toHaveLength(1);
    const outcome = outcomes[0];
    expect(outcome?.message ?? "").not.toContain("abort=");
    expect(outcome?.outcome).toBe(Outcome.COMPLETED);
  }, 30_000);
});

describe("Supervisor crash recovery", () => {
  let sandboxParent: string;
  let walDir: string;
  let probes: Probes;

  beforeEach(() => {
    sandboxParent = mkdtempSync(join(tmpdir(), "harness-sup-"));
    walDir = mkdtempSync(join(tmpdir(), "harness-wal-"));
    probes = new Probes();
  });
  afterEach(() => {
    rmSync(sandboxParent, { recursive: true, force: true });
    rmSync(walDir, { recursive: true, force: true });
  });

  it("replays a crashed turn's unacked WAL events as after_terminal after Welcome", async () => {
    // Simulate a crashed supervisor: a prior turn wrote an event to the WAL but
    // never acked it (the supervisor died mid-turn). The WAL file persists.
    const payload: TurnEventPayload = {
      case: "assistantMessage",
      value: create(AssistantMessageSchema, { text: "crashed-mid-turn" }),
    };
    const dead = new EventOutbox(walDir, "turn-crash", 100);
    dead.append(payload); // seq 1, WAL'd, never acked
    dead.close(); // release the fd; the WAL file remains

    const received: WorkerMessage[] = [];
    let replayDone!: () => void;
    const replayed = new Promise<void>((r) => (replayDone = r));
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 2n }) },
          });
          for await (const m of req) {
            received.push(m);
            if (m.kind.case === "turnEvent") {
              replayDone();
              // ACK the replayed audit tail through its sequence so the WAL is deleted.
              yield create(ControlMessageSchema, {
                kind: {
                  case: "eventAck",
                  value: create(EventAckSchema, {
                    turnId: m.kind.value.turnId,
                    throughSequence: m.kind.value.sequence,
                  }),
                },
              });
            }
          }
        },
      });
    });

    // Constructing the supervisor loads the unfinished WAL (EventOutbox.recover).
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, {
        kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) },
      }),
      childFactory: () => fakeChild([], Outcome.COMPLETED), // not used (no AssignTurn)
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
    });

    const runP = sup.run();
    await replayed; // the crashed turn's event was replayed after Welcome
    await sup.drain();
    await runP;

    const replayedEvent = received.find(
      (m) => m.kind.case === "turnEvent" && m.kind.value.kind.case === "assistantMessage",
    );
    expect(replayedEvent).toBeDefined();
    expect(replayedEvent!.kind!.value!.turnId).toBe("turn-crash");
    expect(Number(replayedEvent!.kind!.value!.sequence)).toBe(1);
    // The WAL was deleted after replay (the turn is durably done as after_terminal).
    expect(existsSync(join(walDir, "turn-crash.wal"))).toBe(false);
  });
});

function assignTurn(sandboxId: string, graph = false): ControlMessageLike {
  return create(ControlMessageSchema, {
    kind: {
      case: "assignTurn",
      value: create(AssignTurnSchema, {
        turnId: "turn-1",
        sessionId: "sess-1",
        sandbox: create(SandboxRefSchema, { sandboxId, uid: UID, gid: GID, workingDir: "home" }),
        persona: "you are an agent",
        model: create(ModelConfigSchema, { id: "m", api: "openai-completions", contextWindow: 131072 }),
        workspaceTools: true,
        runId: "run-1",
        message: "classify this email",
        ...(graph ? { workItemId: "work-1", nodeExecutionId: "node-1", nodeKey: "classify", completionOutcomes: ["completed"] } : {}),
      }) as AssignTurn,
    },
  }) as ControlMessageLike;
}

type ControlMessageLike = ReturnType<typeof create<never>> extends never ? never : import("./gen/iterabase/harness/v1/harness_pb.js").ControlMessage;

/** A no-op gateway client (tests that don't exercise tool calls). */
function fakeGatewayClient(): import("./gateway-client.js").GatewayClient {
  return {
    discover: async () => [],
    invokeTool: async () => ({ invocationId: "", state: 0, resultJson: new Uint8Array(), artifactOutputRefs: [], error: { code: "", message: "", retryability: 0, detailsJson: new Uint8Array() }, existingInvocationId: "" }),
    cancelInvocation: async () => ({ state: 0 }),
    resetTransport: () => {},
  };
}

/** A fake child that emits one toolCall RPC request then closes. */
function fakeToolCallChild(toolName: string): Child & { sent: unknown[] } {
  const q = new Q<ChildEvent>();
  q.close();
  const rpcQ = new Q<import("./supervisor.js").ChildRpcRequest>();
  rpcQ.push({ type: "toolCall", requestId: "req-1", toolCallId: "tc-1", toolName, toolVersionDigest: "sha256:xyz", argumentsJson: "{}", idempotencyKey: "tc-1" });
  rpcQ.close();
  const sent: unknown[] = [];
  const result = new Promise<ChildResult>(() => {}); // never resolves (test drains)
  return { abort: () => {}, noteStepCompletionDurable: () => {}, noteAuditChannelEvidence: () => {}, events: q, rpcRequests: rpcQ, rpcSend: (f) => sent.push(f), result, sent } as Child & { sent: unknown[] };
}

/** A fake Child that records every abort reason the supervisor passes and stays
 * in flight until `release()` (HOR-551 caller→reason mapping tests). */
function recordingChild(): Child & { aborts: string[]; aborted: Promise<string>; release: () => void } {
  const q = new Q<ChildEvent>();
  const rpcQ = new Q<import("./supervisor.js").ChildRpcRequest>();
  rpcQ.close();
  const aborts: string[] = [];
  let resolveAborted!: (reason: string) => void;
  const aborted = new Promise<string>((r) => (resolveAborted = r));
  let finish!: () => void;
  const result = new Promise<ChildResult>((resolve) => {
    finish = () => {
      q.close();
      resolve({ outcome: Outcome.ABORTED });
    };
  });
  return {
    abort: (reason: string) => {
      aborts.push(reason);
      resolveAborted(reason);
    },
    noteStepCompletionDurable: () => {}, noteAuditChannelEvidence: () => {},
    events: q,
    rpcRequests: rpcQ,
    rpcSend: () => {},
    rpcOnDrain: () => () => {},
    result,
    aborts,
    aborted,
    release: () => finish(),
  } as Child & { aborts: string[]; aborted: Promise<string>; release: () => void };
}

describe("Supervisor RPC dispatch (HOR-395)", () => {
  let sandboxParent: string;
  let walDir: string;
  let probes: Probes;
  beforeEach(() => {
    sandboxParent = mkdtempSync(join(tmpdir(), "harness-sup-rpc-"));
    walDir = mkdtempSync(join(tmpdir(), "harness-wal-rpc-"));
    // The supervisor provisions the sandbox itself (HOR-245).
    probes = new Probes();
  });
  afterEach(() => {
    rmSync(sandboxParent, { recursive: true, force: true });
    rmSync(walDir, { recursive: true, force: true });
  });

  it("fails closed on an unassigned tool request (no gateway invoke)", async () => {
    let assignTurnSent = false;
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, { kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) } as never });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn("sess-a");
            }
          }
        },
      });
    });
    let invoked = false;
    const child = fakeToolCallChild("graph.never_permitted");
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: () => child,
      probes,
      transport: () => transport,
      gatewayClient: {
        discover: async () => [], // empty effective set → tool is unassigned
        invokeTool: async () => { invoked = true; return { invocationId: "", state: 0, resultJson: new Uint8Array(), artifactOutputRefs: [], error: undefined, existingInvocationId: "" }; },
        cancelInvocation: async () => ({ state: 0 }),
        resetTransport: () => {},
      },
      modelStream: fakeModelStream(),
    });
    const runP = sup.run();
    await new Promise((r) => setTimeout(r, 200));
    await sup.drain();
    await runP.catch(() => {});
    const sent = child.sent;
    const toolResult = sent.find((f) => (f as { type?: string }).type === "toolResult") as { isError: boolean; errorMessage?: string } | undefined;
    expect(toolResult).toBeDefined();
    expect(toolResult?.isError).toBe(true);
    expect(toolResult?.errorMessage).toContain("not permitted");
    expect(invoked).toBe(false);
  }, 5_000);

  it("rejects customer-system tool calls ordered after complete_step", async () => {
    let assignTurnSent = false;
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, { kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) } as never });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn("sess-a", true);
            }
          }
        },
      });
    });
    const events = new Q<ChildEvent>();
    events.close();
    const requests = new Q<import("./supervisor.js").ChildRpcRequest>();
    requests.push({ type: "stepCompletion", requestId: "complete", outcome: "completed", summary: "done", outputJson: "{}", artifactRefs: [] });
    requests.push({ type: "toolCall", requestId: "after", toolCallId: "tc-after", toolName: "graph.write", toolVersionDigest: "sha256:xyz", argumentsJson: "{}" });
    requests.close();
    const sent: unknown[] = [];
    const child: Child = { abort: () => {}, noteStepCompletionDurable: () => {}, noteAuditChannelEvidence: () => {}, events, rpcRequests: requests, rpcSend: (frame) => sent.push(frame), result: new Promise<ChildResult>(() => {}) };
    let invoked = false;
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: () => child,
      probes,
      transport: () => transport,
      gatewayClient: {
        discover: async () => [{ name: "graph.write", version: "1", digest: "sha256:xyz", description: "write", inputSchema: {}, effectClass: "idempotent_write" as const }],
        invokeTool: async () => { invoked = true; return { invocationId: "", state: 0, resultJson: new Uint8Array(), artifactOutputRefs: [], error: undefined, existingInvocationId: "" }; },
        cancelInvocation: async () => ({ state: 0 }),
        resetTransport: () => {},
      },
      modelStream: fakeModelStream(),
    });
    const runP = sup.run();
    await new Promise((r) => setTimeout(r, 200));
    await sup.drain();
    await runP.catch(() => {});
    expect(sent.some((f) => (f as { type?: string }).type === "stepCompletionAck")).toBe(true);
    const blocked = sent.find((f) => (f as { requestId?: string }).requestId === "after") as { isError?: boolean; errorMessage?: string } | undefined;
    expect(blocked?.isError).toBe(true);
    expect(blocked?.errorMessage).toContain("disabled after complete_step");
    expect(invoked).toBe(false);
  }, 5_000);

  it("HOR-612: an fd-3 audit backlog overflow notice aborts with the child's channel evidence", async () => {
    let assignTurnSent = false;
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, { kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) } as never });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn("sess-a");
            }
          }
        },
      });
    });
    const evidence = {
      queuedFrames: 12,
      queuedBytes: 340_000,
      sinceLastWriteMs: 6_123,
      stalledMs: 5_000,
      lastBlockedFrameKind: "event" as const,
      droppedTokenDeltas: 42,
      coalescedHeartbeats: 3,
      overflowed: true,
    };
    const events = new Q<ChildEvent>();
    events.close();
    const requests = new Q<import("./supervisor.js").ChildRpcRequest>();
    requests.push({ type: "auditBacklogOverflow", requestId: "audit-backlog-overflow", evidence });
    requests.close();
    const aborts: string[] = [];
    const noted: unknown[] = [];
    const child: Child = {
      abort: (reason: string) => aborts.push(reason),
      noteStepCompletionDurable: () => {},
      noteAuditChannelEvidence: (e) => noted.push(e),
      events,
      rpcRequests: requests,
      rpcSend: () => {},
      result: new Promise<ChildResult>(() => {}),
    };
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: () => child,
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
    });
    const runP = sup.run();
    await new Promise((r) => setTimeout(r, 200));
    await sup.drain();
    await runP.catch(() => {});
    // The child's fail-closed notice is recorded as channel evidence and maps to
    // a distinct abort reason (never a bare stale-heartbeat reap). The later
    // `worker_drain` abort from the test teardown keeps the original reason.
    expect(aborts[0]).toBe("audit_backlog_overflow");
    expect(noted).toEqual([evidence]);
  }, 5_000);

  it("rejects a duplicate active requestId fail-closed (bounded in-flight, unambiguous cancellation)", async () => {
    let assignTurnSent = false;
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, { kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) } as never });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn("sess-a");
            }
          }
        },
      });
    });
    // Two toolCalls with the SAME requestId; the first is kept in-flight by a
    // gateway whose invokeTool never resolves, so the second must be rejected
    // as a duplicate instead of launching a second upstream call.
    const q = new Q<ChildEvent>();
    q.close();
    const rpcQ = new Q<import("./supervisor.js").ChildRpcRequest>();
    rpcQ.push({ type: "toolCall", requestId: "dup", toolCallId: "tc-1", toolName: "graph.read", toolVersionDigest: "sha256:xyz", argumentsJson: "{}", idempotencyKey: "tc-1" });
    rpcQ.push({ type: "toolCall", requestId: "dup", toolCallId: "tc-2", toolName: "graph.read", toolVersionDigest: "sha256:xyz", argumentsJson: "{}", idempotencyKey: "tc-2" });
    rpcQ.close();
    const sent: unknown[] = [];
    const child: Child & { sent: unknown[] } = { abort: () => {}, noteStepCompletionDurable: () => {}, noteAuditChannelEvidence: () => {}, events: q, rpcRequests: rpcQ, rpcSend: (f) => sent.push(f), result: new Promise<ChildResult>(() => {}), sent } as Child & { sent: unknown[] };
    let invokeCount = 0;
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: () => child,
      probes,
      transport: () => transport,
      gatewayClient: {
        discover: async () => [{ name: "graph.read", version: "1", digest: "sha256:xyz", description: "", inputSchema: {}, effectClass: "read_only" as const }],
        invokeTool: async () => { invokeCount += 1; return new Promise(() => {}); }, // never resolves
        cancelInvocation: async () => ({ state: 0 }),
        resetTransport: () => {},
      },
      modelStream: fakeModelStream(),
    });
    const runP = sup.run();
    await new Promise((r) => setTimeout(r, 200));
    await sup.drain();
    await runP.catch(() => {});
    const dupResult = sent.find((f) => (f as { type?: string; errorMessage?: string }).type === "toolResult" && (f as { errorMessage?: string }).errorMessage?.includes("duplicate")) as { isError: boolean; errorMessage?: string } | undefined;
    expect(dupResult).toBeDefined();
    expect(dupResult?.isError).toBe(true);
    // Only the first request launched an upstream call; the duplicate did not.
    expect(invokeCount).toBe(1);
  }, 5_000);
});

/**
 * HOR-551 caller→reason mapping. The child-process unit tests call
 * child.abort("<reason>") directly; these assert that the supervisor's three
 * explicit abort callers actually pass their distinct reason across the
 * boundary, so a future refactor cannot silently drop or mis-map it.
 */
describe("Supervisor abort reason mapping (HOR-551)", () => {
  let sandboxParent: string;
  let walDir: string;
  let sandboxId: string;
  let probes: Probes;

  beforeEach(() => {
    sandboxParent = mkdtempSync(join(tmpdir(), "harness-sup-abort-"));
    walDir = mkdtempSync(join(tmpdir(), "harness-wal-abort-"));
    sandboxId = "sess-a";
    probes = new Probes();
  });
  afterEach(() => {
    rmSync(sandboxParent, { recursive: true, force: true });
    rmSync(walDir, { recursive: true, force: true });
  });

  it("AbortTurn passes control_plane_cancel to the child abort", async () => {
    let assignTurnSent = false;
    let childCreated!: () => void;
    const created = new Promise<void>((r) => (childCreated = r));
    const child = recordingChild();
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) as never } as never,
          });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn(sandboxId);
              await created; // the supervisor now owns an active turn
              yield create(ControlMessageSchema, { kind: { case: "abortTurn", value: create(AbortTurnSchema, { turnId: "turn-1" }) } });
            } else if (m.kind.case === "turnEvent" && m.kind.value.kind.case === "workerOutcome") {
              yield create(ControlMessageSchema, {
                kind: { case: "eventAck", value: create(EventAckSchema, { turnId: m.kind.value.turnId, throughSequence: m.kind.value.sequence }) },
              });
            }
          }
        },
      });
    });
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: () => { childCreated(); return child; },
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
    });
    const runP = sup.run();
    await child.aborted;
    expect(child.aborts).toEqual(["control_plane_cancel"]);
    child.release();
    await sup.drain();
    await runP;
  }, 10_000);

  it("drain passes worker_drain to the child abort", async () => {
    let assignTurnSent = false;
    let childCreated!: () => void;
    const created = new Promise<void>((r) => (childCreated = r));
    const child = recordingChild();
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) as never } as never,
          });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn(sandboxId);
            }
          }
        },
      });
    });
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: () => { childCreated(); return child; },
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
    });
    const runP = sup.run();
    await created; // currentChild is set before this continuation resumes
    const drainP = sup.drain();
    await child.aborted;
    expect(child.aborts).toEqual(["worker_drain"]);
    child.release();
    await drainP;
    await runP;
  }, 10_000);

  it("stream loss passes stream_loss to the child abort", async () => {
    let assignTurnSent = false;
    let childCreated!: () => void;
    const created = new Promise<void>((r) => (childCreated = r));
    const child = recordingChild();
    const transport = createRouterTransport((router) => {
      router.service(Harness, {
        async *work(req) {
          yield create(ControlMessageSchema, {
            kind: { case: "welcome", value: create(WelcomeSchema, { fencingGeneration: 1n }) as never } as never,
          });
          for await (const m of req) {
            if (m.kind.case === "ready" && !assignTurnSent) {
              assignTurnSent = true;
              yield assignTurn(sandboxId);
              await created;
              return; // control stream ends while the turn is active → fail-closed
            }
          }
        },
      });
    });
    const sup = new Supervisor({
      cfg: makeCfg(sandboxParent, walDir),
      hello: create(WorkerMessageSchema, { kind: { case: "hello", value: create(HelloSchema, { workerId: "pod-1", poolId: "pool-1" }) } }),
      childFactory: () => { childCreated(); return child; },
      probes,
      transport: () => transport,
      gatewayClient: fakeGatewayClient(),
      modelStream: fakeModelStream(),
    });
    const runP = sup.run();
    await child.aborted;
    expect(child.aborts).toEqual(["stream_loss"]);
    child.release();
    await sup.drain();
    await runP.catch(() => {});
  }, 10_000);
});

/** A no-op model stream (tests that don't exercise model calls). */
function fakeModelStream(): typeof import("./model-bridge.js").streamModel {
  return async () => {};
}
