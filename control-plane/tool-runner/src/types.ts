import type { ArtifactRef } from "./gen/iterabase/gateway/v1/gateway_pb.js";

export interface ToolIdentity { name: string; version: string }

export interface ToolErrorShape {
  code: string;
  message: string;
  retryable?: boolean;
  details?: unknown;
}

export class ToolError extends Error {
  constructor(public readonly detail: ToolErrorShape) {
    super(detail.message);
    this.name = "ToolError";
  }
}

export interface ToolArtifactAPI {
  read(ref: ArtifactRef): AsyncIterable<Uint8Array>;
  // filename's extension must agree with the detected type (HOR-461); the
  // output is quarantined and usable by consumers only after clearance.
  write(input: { mimeType: string; bytes: AsyncIterable<Uint8Array>; filename?: string; expectedSizeBytes?: bigint; expectedDigest?: string }): Promise<ArtifactRef>;
}

export interface ToolInvocationContext {
  invocationId: string;
  idempotencyKey: string;
  credentials: Readonly<Record<string, unknown>>;
  artifacts: ToolArtifactAPI;
  signal: AbortSignal;
}

export interface ToolResult {
  result: unknown;
  artifactRefs?: ArtifactRef[];
}

export interface ToolModule {
  identity: ToolIdentity;
  invoke(context: Readonly<ToolInvocationContext>, argumentsValue: unknown): Promise<ToolResult>;
}
