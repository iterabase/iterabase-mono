#!/usr/bin/env node
// Invoke one released product tool bundle directly, for the live Microsoft 365
// checks (HOR-358, DES-HOR-358-04). Not shipped in the image.
//
//   M365_TOOL_ACCESS_TOKEN=<token> node scripts/invoke-product-tool.mjs <tool-dir> '<arguments JSON>'
//
// Prints {"result": ..., "artifacts": [{mimeType, sizeBytes}]} or
// {"error": {code, message, retryable, details}}. Artifact bytes stay in memory.
import { pathToFileURL } from "node:url";
import { resolve } from "node:path";

const [dir, rawArgs] = process.argv.slice(2);
const token = process.env.M365_TOOL_ACCESS_TOKEN;
if (!dir || !rawArgs || !token) {
  console.error("usage: M365_TOOL_ACCESS_TOKEN=... invoke-product-tool.mjs <tool-dir> '<arguments JSON>'");
  process.exit(2);
}
const tool = await import(pathToFileURL(resolve(dir, "index.mjs")).href);
const artifacts = [];
const context = Object.freeze({
  invocationId: "live-check",
  idempotencyKey: "live-check",
  credentials: Object.freeze({ microsoft365: { scheme: "oauth_client_credentials", accessToken: token } }),
  signal: AbortSignal.timeout(60_000),
  artifacts: {
    read() { throw new Error("artifact reads are not used by the live checks"); },
    async write(input) {
      let size = 0;
      for await (const chunk of input.bytes) size += chunk.byteLength;
      artifacts.push({ mimeType: input.mimeType, sizeBytes: size });
      return { artifactId: `live-${artifacts.length}` };
    },
  },
});
try {
  const out = await tool.invoke(context, JSON.parse(rawArgs));
  process.stdout.write(JSON.stringify({ result: out.result, artifacts }));
} catch (error) {
  if (error?.name !== "ToolError") throw error;
  process.stdout.write(JSON.stringify({ error: { code: error.code, message: error.message, retryable: error.retryable ?? false, details: error.details ?? null } }));
}
