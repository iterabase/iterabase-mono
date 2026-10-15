import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { loadLayers } from "./manifest.js";
import type { LoadedTool } from "./manifest.js";

// The platform mailbox product tools (HOR-358). Graph is replaced by a stub
// fetch; the bundles are the exact released bytes.
const productRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "tools", "product");
const TOKEN = "access-token-secret";

let read: LoadedTool;
let send: LoadedTool;
beforeAll(async () => {
  const generation = await loadLayers(productRoot, null, productRoot, { revision: "test", artifactDigest: "test" });
  read = generation.tools.find((tool) => tool.manifest.name === "platform.mailbox.read")!;
  send = generation.tools.find((tool) => tool.manifest.name === "platform.mailbox.send")!;
});
afterEach(() => vi.unstubAllGlobals());

interface Call { method: string; url: string; headers: Record<string, string>; body?: unknown }
type Route = (call: Call) => Response | Promise<Response>;

function stubGraph(route: Route): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal("fetch", async (url: string, init: RequestInit = {}) => {
    const call: Call = { method: init.method ?? "GET", url, headers: init.headers as Record<string, string>,
      body: typeof init.body === "string" ? JSON.parse(init.body) : undefined };
    calls.push(call);
    return route(call);
  });
  return calls;
}

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { "content-type": "application/json" } });
}

function context(writes: Array<{ mimeType: string; bytes: Buffer }> = []) {
  return {
    invocationId: "inv-1",
    idempotencyKey: "idem-1",
    credentials: { microsoft365: { scheme: "oauth_client_credentials", accessToken: TOKEN } },
    signal: new AbortController().signal,
    artifacts: {
      read: () => { throw new Error("unused"); },
      async write(input: { mimeType: string; bytes: AsyncIterable<Uint8Array> }) {
        const chunks: Uint8Array[] = [];
        for await (const chunk of input.bytes) chunks.push(chunk);
        writes.push({ mimeType: input.mimeType, bytes: Buffer.concat(chunks) });
        return { artifactId: `artifact-${writes.length}` };
      },
    },
  } as never;
}

describe("platform.mailbox.read", () => {
  it("declares a read-only tool bound to the Microsoft 365 OAuth slot", () => {
    expect(read.manifest.effectClass).toBe("read_only");
    expect(read.manifest.credentialSlots).toEqual([{ name: "microsoft365", scheme: "oauth_client_credentials", required: true }]);
    expect(read.manifest.artifactCapabilities?.writesArtifacts).toBe(true);
  });

  it("returns reply-matching identity, a bounded text body and the HTML body as an artifact", async () => {
    const longText = "a".repeat(40 * 1024);
    const calls = stubGraph((call) => {
      if (call.url.includes("$select=body")) {
        return call.headers.Prefer.includes('"text"')
          ? json({ body: { contentType: "text", content: longText } })
          : json({ body: { contentType: "html", content: "<p>Hello</p>" } });
      }
      return json({
        id: "AAMk-immutable", internetMessageId: "<m1@contoso.com>", conversationId: "conv-1", subject: "Quote",
        from: { emailAddress: { name: "Ana", address: "ana@contoso.com" } }, toRecipients: [{ emailAddress: { address: "quotes@customer.pt" } }],
        receivedDateTime: "2026-10-14T10:00:00Z", hasAttachments: true,
        internetMessageHeaders: [{ name: "In-Reply-To", value: " <m0@customer.pt> " }, { name: "References", value: "<a@x> <m0@customer.pt>" }],
      });
    });
    const writes: Array<{ mimeType: string; bytes: Buffer }> = [];
    const out = await read.module.invoke(context(writes), { mailbox: "quotes@customer.pt", operation: "get_message", messageId: "AAMk/1=" });
    const result = out.result as Record<string, unknown>;
    expect(result).toMatchObject({
      id: "AAMk-immutable", internetMessageId: "<m1@contoso.com>", conversationId: "conv-1",
      inReplyTo: "<m0@customer.pt>", references: ["<a@x>", "<m0@customer.pt>"],
      from: { name: "Ana", address: "ana@contoso.com" }, bodyTextTruncated: true, bodyHtml: { artifactId: "artifact-1", sizeBytes: 12 },
    });
    expect(Buffer.byteLength(result.bodyText as string)).toBe(32 * 1024);
    expect(writes).toEqual([{ mimeType: "text/html", bytes: Buffer.from("<p>Hello</p>") }]);
    expect(out.artifactRefs).toEqual([{ artifactId: "artifact-1" }]);
    for (const call of calls) {
      expect(call.url.startsWith("https://graph.microsoft.com/v1.0/users/quotes%40customer.pt/messages/AAMk%2F1%3D")).toBe(true);
      expect(call.headers.Authorization).toBe(`Bearer ${TOKEN}`);
      expect(call.headers.Prefer).toContain('IdType="ImmutableId"');
    }
    expect(JSON.stringify(out)).not.toContain(TOKEN);
  });

  it("writes attachment bytes to an artifact and an attached message as message/rfc822", async () => {
    stubGraph((call) => call.url.endsWith("/$value")
      ? new Response(Buffer.from("From: a@b\r\n\r\nhi"))
      : json({ id: "att-1", name: "fwd.eml", contentType: "application/octet-stream", size: 14, "@odata.type": "#microsoft.graph.itemAttachment" }));
    const writes: Array<{ mimeType: string; bytes: Buffer }> = [];
    const out = await read.module.invoke(context(writes), { mailbox: "quotes@customer.pt", operation: "get_attachment", messageId: "m", attachmentId: "att-1" });
    expect(out.result).toMatchObject({ id: "att-1", contentType: "message/rfc822", sizeBytes: 15, artifactId: "artifact-1" });
    expect(writes[0].bytes.toString()).toBe("From: a@b\r\n\r\nhi");
  });

  it("refuses an attachment above the read limit before downloading it", async () => {
    const calls = stubGraph(() => json({ id: "att-1", size: 26 * 1024 * 1024, "@odata.type": "#microsoft.graph.fileAttachment" }));
    await expect(read.module.invoke(context(), { mailbox: "q@c.pt", operation: "get_attachment", messageId: "m", attachmentId: "att-1" }))
      .rejects.toMatchObject({ name: "ToolError", code: "attachment_too_large" });
    expect(calls).toHaveLength(1);
  });

  it("cancels an oversized body response before buffering it", async () => {
    // 12 MiB of HTML in 64 KiB chunks against an 8 MiB response limit.
    const chunk = new Uint8Array(64 * 1024).fill(0x61);
    let pulled = 0;
    let cancelled = false;
    const oversized = () => new Response(new ReadableStream({
      pull(controller) {
        if (pulled >= 12 * 1024 * 1024) return controller.close();
        pulled += chunk.byteLength;
        controller.enqueue(chunk);
      },
      cancel() { cancelled = true; },
    }), { status: 200 });
    stubGraph((call) => call.url.includes("$select=body") ? oversized() : json({ id: "m" }));
    await expect(read.module.invoke(context(), { mailbox: "q@c.pt", operation: "get_message", messageId: "m" }))
      .rejects.toMatchObject({ name: "ToolError", code: "message_too_large" });
    expect(cancelled).toBe(true);
    expect(pulled).toBeLessThan(9 * 1024 * 1024);
  });

  it("refuses a response whose declared length exceeds the limit and cancels it", async () => {
    let cancelled = false;
    stubGraph(() => new Response(new ReadableStream({ cancel() { cancelled = true; } }),
      { status: 200, headers: { "content-length": String(2 * 1024 * 1024) } }));
    await expect(read.module.invoke(context(), { mailbox: "q@c.pt", operation: "list_attachments", messageId: "m" }))
      .rejects.toMatchObject({ code: "message_too_large" });
    expect(cancelled).toBe(true);
  });

  it("lists attachments without content", async () => {
    stubGraph(() => json({ value: [{ id: "a1", name: "rfq.pdf", contentType: "application/pdf", size: 1024, isInline: false, "@odata.type": "#microsoft.graph.fileAttachment" }] }));
    const out = await read.module.invoke(context(), { mailbox: "q@c.pt", operation: "list_attachments", messageId: "m" });
    expect(out.result).toEqual({ attachments: [{ id: "a1", name: "rfq.pdf", contentType: "application/pdf", sizeBytes: 1024, isInline: false, kind: "file" }], truncated: false });
  });

  it("maps a tenant denial to access_denied without echoing provider text", async () => {
    stubGraph(() => json({ error: { code: "ErrorAccessDenied", message: `secret detail ${TOKEN}` } }, 403));
    const error = await read.module.invoke(context(), { mailbox: "other@c.pt", operation: "get_message", messageId: "m" }).catch((e: unknown) => e);
    expect(error).toMatchObject({ name: "ToolError", code: "access_denied", retryable: false });
    expect(String((error as Error).message)).not.toContain("secret detail");
  });

  it("fails closed without a bound credential", async () => {
    await expect(read.module.invoke({ ...(context() as object), credentials: {} } as never, { mailbox: "q@c.pt", operation: "get_message", messageId: "m" }))
      .rejects.toMatchObject({ code: "credential_unavailable" });
  });
});

describe("platform.mailbox.send", () => {
  it("declares a non-idempotent write with exact EN and PT consequence templates", () => {
    expect(send.manifest.effectClass).toBe("non_idempotent_write");
    expect(send.manifest.consequenceSummaryTemplate?.localizedTemplates).toEqual({
      en: "Send the email \"{{subject}}\" from {{mailbox}} to {{to}}",
      pt: "Enviar o email \"{{subject}}\" de {{mailbox}} para {{to}}",
    });
  });

  it("creates a draft, sends it and returns the identifiers known before the send", async () => {
    const calls = stubGraph((call) => call.url.endsWith("/send")
      ? new Response(null, { status: 202 })
      : json({ id: "draft-1", internetMessageId: "<sent-1@customer.pt>", conversationId: "conv-9" }, 201));
    const out = await send.module.invoke(context(), { mailbox: "quotes@customer.pt", to: "supplier@acme.com", subject: "RFQ [IB-7Q2]", body: "Hello" });
    expect(out.result).toEqual({ status: "sent", providerMessageId: "draft-1", internetMessageId: "<sent-1@customer.pt>", conversationId: "conv-9" });
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      "POST https://graph.microsoft.com/v1.0/users/quotes%40customer.pt/messages",
      "POST https://graph.microsoft.com/v1.0/users/quotes%40customer.pt/messages/draft-1/send",
    ]);
    expect(calls[0].body).toEqual({ subject: "RFQ [IB-7Q2]", body: { contentType: "Text", content: "Hello" }, toRecipients: [{ emailAddress: { address: "supplier@acme.com" } }] });
  });

  it("replies through createReply and reads missing identifiers before sending", async () => {
    const calls = stubGraph((call) => {
      if (call.url.endsWith("/send")) return new Response(null, { status: 202 });
      if (call.url.includes("createReply")) return json({ id: "draft-2" }, 201);
      return json({ id: "draft-2", internetMessageId: "<re@customer.pt>", conversationId: "conv-1" });
    });
    const out = await send.module.invoke(context(), { mailbox: "q@customer.pt", to: "a@acme.com", subject: "RE: RFQ", body: "<p>Yes</p>", bodyType: "html", replyToMessageId: "orig-1" });
    expect(out.result).toMatchObject({ internetMessageId: "<re@customer.pt>", conversationId: "conv-1" });
    expect(calls[0].url).toContain("/messages/orig-1/createReply");
    expect(calls[0].body).toMatchObject({ message: { body: { contentType: "HTML", content: "<p>Yes</p>" } } });
    expect(calls[1].method).toBe("GET");
    expect(calls[2].url).toContain("/messages/draft-2/send");
  });

  it("re-reads missing identifiers and never sends without both", async () => {
    let reads = 0;
    const calls = stubGraph((call) => {
      if (call.url.endsWith("/send")) return new Response(null, { status: 202 });
      if (call.method === "POST") return json({ id: "draft-1" }, 201);
      reads++;
      return json({ id: "draft-1", internetMessageId: "<x@c.pt>" }); // conversationId never arrives
    });
    await expect(send.module.invoke(context(), { mailbox: "q@c.pt", to: "a@b.com", subject: "s", body: "b" }))
      .rejects.toMatchObject({ code: "not_sent", details: { providerMessageId: "draft-1" } });
    expect(reads).toBe(3);
    expect(calls.some((c) => c.url.endsWith("/send"))).toBe(false);
  });

  it("sends once the identifiers arrive on a later read", async () => {
    let reads = 0;
    stubGraph((call) => {
      if (call.url.endsWith("/send")) return new Response(null, { status: 202 });
      if (call.method === "POST") return json({ id: "draft-1" }, 201);
      reads++;
      return json(reads < 2 ? { id: "draft-1" } : { id: "draft-1", internetMessageId: "<x@c.pt>", conversationId: "conv" });
    });
    const out = await send.module.invoke(context(), { mailbox: "q@c.pt", to: "a@b.com", subject: "s", body: "b" });
    expect(out.result).toMatchObject({ status: "sent", internetMessageId: "<x@c.pt>", conversationId: "conv" });
  });

  it("reports not_sent when the draft cannot be created", async () => {
    stubGraph(() => json({ error: { code: "ErrorInvalidRecipients" } }, 400));
    await expect(send.module.invoke(context(), { mailbox: "q@c.pt", to: "a@b.com", subject: "s", body: "b" }))
      .rejects.toMatchObject({ code: "provider_error", message: expect.stringContaining("nothing was sent") });
  });

  it("reports outcome_unknown with the draft identity when the send outcome is uncertain", async () => {
    stubGraph((call) => call.url.endsWith("/send")
      ? new Response(null, { status: 503 })
      : json({ id: "draft-1", internetMessageId: "<x@c.pt>", conversationId: "conv" }, 201));
    await expect(send.module.invoke(context(), { mailbox: "q@c.pt", to: "a@b.com", subject: "s", body: "b" }))
      .rejects.toMatchObject({ code: "outcome_unknown", retryable: false, details: { providerMessageId: "draft-1", internetMessageId: "<x@c.pt>", conversationId: "conv" } });
  });

  it("reports not_sent when Graph definitively rejects the send", async () => {
    stubGraph((call) => call.url.endsWith("/send")
      ? new Response(null, { status: 400 })
      : json({ id: "draft-1", internetMessageId: "<x@c.pt>", conversationId: "conv" }, 201));
    await expect(send.module.invoke(context(), { mailbox: "q@c.pt", to: "a@b.com", subject: "s", body: "b" }))
      .rejects.toMatchObject({ code: "not_sent", retryable: false });
  });
});
