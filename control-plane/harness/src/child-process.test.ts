import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { spawn } from "node:child_process";
import { writeFileSync, mkdtempSync, rmSync, mkdirSync, chmodSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { create } from "@bufbuild/protobuf";
import {
  AssignTurnSchema,
  AssistantMessageSchema,
  ModelConfigSchema,
  Outcome,
  SandboxRefSchema,
  TurnEventSchema,
  type AssignTurn,
} from "./gen/iterabase/harness/v1/harness_pb.js";
import { createChildFactory } from "./child-process.js";
import type { HarnessConfig } from "./config.js";

const UID = process.getuid();
const GID = process.getgid();
const PI_ENTRY_URL = pathToFileURL(join(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "node_modules",
  "@earendil-works",
  "pi-coding-agent",
  "dist",
  "index.js",
)).href;

// A stub child: writes one AssistantMessage event + a COMPLETED result over
// the framed fd-3 IPC channel (4-byte BE length + JSON), then exits.
const STUB_CHILD = `const fs = require('fs');
function frame(obj){const j=Buffer.from(JSON.stringify(obj));const h=Buffer.alloc(4);h.writeUInt32BE(j.length,0);fs.writeSync(3,Buffer.concat([h,j]));}
frame({type:'heartbeat'});
frame({type:'event',event:{turnId:'t',sequence:'0',timestampMs:'0',assistantMessage:{text:'hi'}}});
frame({type:'result',outcome:1});
`;

let dir: string;
let stubScript: string;
beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "harness-cp-"));
  stubScript = join(dir, "stub-child.cjs");
  writeFileSync(stubScript, STUB_CHILD);
  const root = join(dir, "sess-a");
  mkdirSync(root, { recursive: true });
  for (const sub of ["home", "tmp", "session", "workspace"]) mkdirSync(join(root, sub), { recursive: true });
  chmodSync(root, 0o700);
});
afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

function cfg(): HarnessConfig {
  return {
    controlPlane: { url: "", serverName: "" },
    worker: { workerId: "pod-1", poolId: "pool-1" },
    tls: { cert: "", key: "", ca: "" },
    sandboxRoot: dir,
    piDirs: [],
    toolGateway: { url: "https://localhost:8442", serverName: "tool-gateway" },
    inferenceGateway: { url: "https://localhost:8443", serverName: "inference-gateway" },
    walDir: dir,
    probe: { port: 0 },
    transport: { http2PingIntervalMs: 30000, http2PingTimeoutMs: 10000 },
    reconnect: { initialBackoffMs: 1, maxBackoffMs: 2, resetAfterMs: 1000 },
    child: { livenessIntervalMs: 1000, abortGraceMs: 1000 },
    outbox: { bound: 4096 },
    modelRetry: { maxAttempts: 3 },
    tokenDelta: { sendBufferBytes: 1048576 },
  } as HarnessConfig;
}

function assignment(): AssignTurn {
  return create(AssignTurnSchema, {
    turnId: "turn-1",
    sessionId: "sess-1",
    sandbox: create(SandboxRefSchema, { sandboxId: "sess-a", uid: UID, gid: GID, workingDir: "home" }),
    persona: "you are an agent",
    model: create(ModelConfigSchema, { id: "m", api: "openai-completions", contextWindow: 131072 }),
    workspaceTools: true,
    runId: "run-1",
    message: "hi",
  });
}

// Injectable launch that spawns node directly (bypasses setpriv — Linux-only — for macOS tests).
function launchStub(opts: Parameters<Parameters<typeof createChildFactory>[2]>[0]) {
  return spawn(process.execPath, [opts.script], {
    cwd: join(opts.sandboxRoot, opts.workingDir),
    stdio: opts.stdio,
    env: { ...opts.env, PATH: process.env.PATH ?? "" },
  });
}

describe("createChildFactory", () => {
  it("spawns the child, relays its event + result over IPC", async () => {
    const factory = createChildFactory(cfg(), stubScript, launchStub);
    const sandbox = { root: join(dir, "sess-a"), home: join(dir, "sess-a/home"), tmp: join(dir, "sess-a/tmp"), session: join(dir, "sess-a/session"), workspace: join(dir, "sess-a/workspace") };
    const child = factory(assignment(), sandbox, join(dir, "sess-a/home"));

    const events: import("./supervisor.js").ChildEvent[] = [];
    for await (const ev of child.events) events.push(ev);
    const result = await child.result;

    expect(events).toHaveLength(1);
    expect(events[0]!.payload.case).toBe("assistantMessage");
    expect(events[0]!.payload.case === "assistantMessage" && events[0]!.payload.value.text).toBe("hi");
    expect(result.outcome).toBe(Outcome.COMPLETED);
  });

  it("keeps post-create Pi model refreshes offline even when network is reachable", async () => {
    let catalogRequests = 0;
    const server = createServer((_req, res) => {
      catalogRequests += 1;
      res.writeHead(200, { "content-type": "application/json" });
      res.end("[]");
    });
    await new Promise<void>((resolve, reject) => {
      const onError = (error: Error): void => reject(error);
      server.once("error", onError);
      server.listen(0, "127.0.0.1", () => {
        server.off("error", onError);
        resolve();
      });
    });

    try {
      const address = server.address();
      if (!address || typeof address === "string") throw new Error("test catalog server has no TCP address");
      const refreshScript = join(dir, "offline-refresh.mjs");
      writeFileSync(refreshScript, `
import { writeSync } from "node:fs";
import { ModelRuntime } from ${JSON.stringify(PI_ENTRY_URL)};
function frame(obj) {
  const body = Buffer.from(JSON.stringify(obj));
  const header = Buffer.alloc(4);
  header.writeUInt32BE(body.length, 0);
  writeSync(3, Buffer.concat([header, body]));
}
frame({ type: "heartbeat" });
try {
  const runtime = await ModelRuntime.create({
    modelsPath: null,
    allowModelNetwork: false,
    refreshOnCreate: false,
    catalogBaseUrl: ${JSON.stringify(`http://127.0.0.1:${address.port}`)},
  });
  await runtime.refresh();
  frame({ type: "result", outcome: 1 });
} catch (error) {
  frame({ type: "result", outcome: 2, message: String(error) });
}
`);

      const factory = createChildFactory(cfg(), refreshScript, launchStub);
      const sandbox = { root: join(dir, "sess-a"), home: join(dir, "sess-a/home"), tmp: join(dir, "sess-a/tmp"), session: join(dir, "sess-a/session"), workspace: join(dir, "sess-a/workspace") };
      const child = factory(assignment(), sandbox, sandbox.home);
      const result = await child.result;

      expect(result.outcome, result.message).toBe(Outcome.COMPLETED);
      expect(catalogRequests).toBe(0);
    } finally {
      await new Promise<void>((resolve, reject) => {
        server.close((error) => error ? reject(error) : resolve());
      });
    }
  }, 10_000);

  it("classifies a non-zero exit (no result) as FAILED", async () => {
    const failScript = join(dir, "fail.cjs");
    writeFileSync(failScript, 'process.exit(3);\n');
    const factory = createChildFactory(cfg(), failScript, launchStub);
    const sandbox = { root: join(dir, "sess-a"), home: "", tmp: "", session: "", workspace: "" };
    const child = factory(assignment(), sandbox as never, "");
    const result = await child.result;
    expect(result.outcome).toBe(Outcome.FAILED);
    expect(result.message).toContain("3");
  });

  it("HOR-612: attributes the audit-overflow exit code when the terminal result could not be delivered", async () => {
    // The worst case for the bounded audit channel: fd 3 failed closed (so no
    // `result` frame can ever arrive) and the fd-4 overflow notice did not get
    // through either. The exit code alone must still name the cause.
    const overflowScript = join(dir, "audit-overflow-exit.cjs");
    writeFileSync(overflowScript, "process.exit(8);\n");
    const factory = createChildFactory(cfg(), overflowScript, launchStub);
    const sandbox = { root: join(dir, "sess-a"), home: "", tmp: "", session: "", workspace: "" };
    const child = factory(assignment(), sandbox as never, "");
    const result = await child.result;
    expect(result.outcome).toBe(Outcome.FAILED);
    expect(result.message).toContain("child exit 8");
    expect(result.message).toContain("fd-3 audit backlog overflow");
  });

  it("abort() sends SIGTERM (child exits; classified ABORTED if no result)", async () => {
    const hangScript = join(dir, "hang.cjs");
    writeFileSync(hangScript, 'setInterval(()=>{}, 1000);\n');
    const factory = createChildFactory(cfg(), hangScript, launchStub);
    const sandbox = { root: join(dir, "sess-a"), home: "", tmp: "", session: "", workspace: "" };
    const child = factory(assignment(), sandbox as never, "");
    setTimeout(() => child.abort("control_plane_cancel"), 50);
    const result = await child.result;
    expect(result.outcome).toBe(Outcome.ABORTED);
    expect(result.abort?.reason).toBe("control_plane_cancel");
    expect(result.message).toContain("abort=control_plane_cancel");
  });

  it("fails the turn closed when the fd-5 response backlog overflows (HOR-395 bounded buffering)", async () => {
    // A child that hangs and never reads fd 5: supervisor response frames
    // back up. A runaway dispatch loop that ignores `rpcSend`'s `false` would
    // let Node buffer an unbounded number of frames in supervisor memory; the
    // hard aggregate backlog cap must fail the turn closed instead.
    const noReadScript = join(dir, "noread.cjs");
    writeFileSync(noReadScript, 'setInterval(()=>{}, 1000);\n');
    const factory = createChildFactory(cfg(), noReadScript, launchStub);
    const sandbox = { root: join(dir, "sess-a"), home: "", tmp: "", session: "", workspace: "" };
    const child = factory(assignment(), sandbox as never, "");
    // Flood fd 5 with response frames as a compromised/fast child would force
    // the dispatcher to do. The child never drains, so writes backpressure.
    for (let i = 0; i < 5000; i++) {
      child.rpcSend({ type: "modelEnd", requestId: `r${i}`, status: "error", errorMessage: "x" });
    }
    const result = await Promise.race([
      child.result,
      new Promise<never>((_, rej) => setTimeout(() => rej(new Error("timeout")), 2000)),
    ]);
    expect(result.outcome).toBe(Outcome.FAILED);
    expect(result.message).toMatch(/backlog|not draining/i);
    expect(result.abort?.reason).toBe("rpc_backlog_overflow");
    child.abort("worker_drain");
  }, 5000);

  it("HOR-551: a genuinely stale child still escalates SIGTERM → SIGKILL and resolves ABORTED with diagnostics", async () => {
    // The fix must not convert genuine liveness loss into success: a child that
    // stops heartbeating with no durable complete_step and no result is reaped
    // with bounded escalation, and the abort record identifies the watchdog
    // trigger and the signal sequence.
    const staleCfg = cfg();
    staleCfg.child = { livenessIntervalMs: 500, abortGraceMs: 500 };
    const staleScript = join(dir, "stale.cjs");
    writeFileSync(
      staleScript,
      `const fs = require('fs');
function frame(obj){const j=Buffer.from(JSON.stringify(obj));const h=Buffer.alloc(4);h.writeUInt32BE(j.length,0);fs.writeSync(3,Buffer.concat([h,j]));}
frame({type:'heartbeat'});
process.on('SIGTERM', () => {});
setInterval(()=>{}, 1000);\n`,
    );
    const factory = createChildFactory(staleCfg, staleScript, launchStub);
    const sandbox = { root: join(dir, "sess-a"), home: "", tmp: "", session: "", workspace: "" };
    const child = factory(assignment(), sandbox as never, "");
    const result = await child.result;
    expect(result.outcome).toBe(Outcome.ABORTED);
    expect(result.abort?.reason).toBe("watchdog_stale_heartbeat");
    expect(result.abort?.terminalPhase).toBe(false);
    expect(result.abort?.stepCompletionObserved).toBe(false);
    expect(result.abort?.stepCompletionDurable).toBe(false);
    expect(result.abort?.provisionalResultObserved).toBe(false);
    expect(result.abort?.signals.map((s) => s.signal)).toEqual(["SIGTERM", "SIGKILL"]);
    expect(result.abort?.finalSignal).toBe("SIGKILL");
    expect(result.message).toContain("abort=watchdog_stale_heartbeat");
    expect(result.message).toContain("signals=SIGTERM+SIGKILL");
  }, 10_000);

  it("HOR-612: the abort record carries the last child-reported fd-3 audit-channel evidence", async () => {
    // A stale-heartbeat abort must be attributable from the durable record: the
    // child's last audit-queue snapshot (delivered on its heartbeat) tells an
    // operator whether the liveness channel was already backpressured or the
    // child went silent with a healthy pipe.
    const staleCfg = cfg();
    staleCfg.child = { livenessIntervalMs: 500, abortGraceMs: 500 };
    const staleScript = join(dir, "stale-with-audit.cjs");
    writeFileSync(
      staleScript,
      `const fs = require('fs');
function frame(obj){const j=Buffer.from(JSON.stringify(obj));const h=Buffer.alloc(4);h.writeUInt32BE(j.length,0);fs.writeSync(3,Buffer.concat([h,j]));}
frame({type:'heartbeat',auditQueue:{queuedFrames:9,queuedBytes:12345,sinceLastWriteMs:4321,stalledMs:4000,lastBlockedFrameKind:'tokenDelta',droppedTokenDeltas:7,coalescedHeartbeats:2,overflowed:false}});
process.on('SIGTERM', () => {});
setInterval(()=>{}, 1000);\n`,
    );
    const factory = createChildFactory(staleCfg, staleScript, launchStub);
    const sandbox = { root: join(dir, "sess-a"), home: "", tmp: "", session: "", workspace: "" };
    const child = factory(assignment(), sandbox as never, "");
    const result = await child.result;
    expect(result.outcome).toBe(Outcome.ABORTED);
    expect(result.abort?.reason).toBe("watchdog_stale_heartbeat");
    expect(result.abort?.auditChannel).toEqual({
      queuedFrames: 9,
      queuedBytes: 12_345,
      sinceLastWriteMs: 4_321,
      stalledMs: 4_000,
      lastBlockedFrameKind: "tokenDelta",
      droppedTokenDeltas: 7,
      coalescedHeartbeats: 2,
      overflowed: false,
    });
    expect(result.message).toContain("audit_queue=9f/12345b");
    expect(result.message).toContain("audit_last_blocked=tokenDelta");
    expect(result.message).toContain("audit_dropped_token_deltas=7");
    expect(result.message).toContain("audit_overflowed=false");
  }, 10_000);

  it("HOR-551: the terminal phase gets a bounded post-completion window instead of the liveness window", async () => {
    // A durable complete_step moves the child into its terminal phase: crossing
    // the ordinary liveness window during post-completion teardown must not be
    // reaped as ABORTED, while a clean exit still resolves the provisional
    // COMPLETED (the clean-exit contract is unchanged).
    const terminalCfg = cfg();
    terminalCfg.child = { livenessIntervalMs: 500, abortGraceMs: 1000 };
    const terminalScript = join(dir, "post-completion.cjs");
    writeFileSync(
      terminalScript,
      `const fs = require('fs');
function frame(obj){const j=Buffer.from(JSON.stringify(obj));const h=Buffer.alloc(4);h.writeUInt32BE(j.length,0);fs.writeSync(3,Buffer.concat([h,j]));}
frame({type:'heartbeat'});
setTimeout(() => { frame({type:'result', outcome:1}); process.exit(0); }, 900);\n`,
    );
    const factory = createChildFactory(terminalCfg, terminalScript, launchStub);
    const sandbox = { root: join(dir, "sess-a"), home: "", tmp: "", session: "", workspace: "" };
    const child = factory(assignment(), sandbox as never, "");
    child.noteStepCompletionDurable();
    const result = await child.result;
    expect(result.outcome).toBe(Outcome.COMPLETED);
    expect(result.abort).toBeUndefined();
  }, 10_000);

  it("HOR-551: a child stuck after durable complete_step still fails closed with a distinct post-completion reason", async () => {
    const timeoutCfg = cfg();
    timeoutCfg.child = { livenessIntervalMs: 500, abortGraceMs: 500 };
    const stuckScript = join(dir, "stuck-after-complete.cjs");
    writeFileSync(
      stuckScript,
      `const fs = require('fs');
function frame(obj){const j=Buffer.from(JSON.stringify(obj));const h=Buffer.alloc(4);h.writeUInt32BE(j.length,0);fs.writeSync(3,Buffer.concat([h,j]));}
frame({type:'heartbeat'});
process.on('SIGTERM', () => {});
setInterval(()=>{}, 1000);\n`,
    );
    const factory = createChildFactory(timeoutCfg, stuckScript, launchStub);
    const sandbox = { root: join(dir, "sess-a"), home: "", tmp: "", session: "", workspace: "" };
    const child = factory(assignment(), sandbox as never, "");
    child.noteStepCompletionDurable();
    const result = await child.result;
    expect(result.outcome).toBe(Outcome.ABORTED);
    expect(result.abort?.reason).toBe("watchdog_post_completion_timeout");
    expect(result.abort?.terminalPhase).toBe(true);
    expect(result.abort?.stepCompletionDurable).toBe(true);
    expect(result.abort?.effectiveWindowMs).toBe(1000);
    expect(result.abort?.signals.map((s) => s.signal)).toEqual(["SIGTERM", "SIGKILL"]);
    expect(result.message).toContain("abort=watchdog_post_completion_timeout");
    expect(result.message).toContain("complete_step=durable");
  }, 10_000);
});
