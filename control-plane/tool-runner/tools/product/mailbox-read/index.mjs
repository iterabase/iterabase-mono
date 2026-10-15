// Bounded Microsoft 365 mailbox reads (HOR-358, DES-HOR-358-02). The gateway
// has already limited `mailbox` to an active mailbox on this tool's credential
// slot (DES-HOR-358-03) and hands the runner only a short-lived access token.
// Bodies are bounded inline; HTML bodies and attachment bytes go to the
// artifact service, never into the result.
export const identity = { name: "platform.mailbox.read", version: "1.0.0" };

const GRAPH = "https://graph.microsoft.com/v1.0";
const MAX_TEXT_BYTES = 32 * 1024;
const MAX_HTML_BYTES = 4 * 1024 * 1024;
const MAX_ATTACHMENT_BYTES = 25 * 1024 * 1024;
const MAX_ATTACHMENTS = 100;
// Provider responses are read through these byte limits before any parsing.
const MAX_JSON_BYTES = 1024 * 1024;
// A body response carries the body JSON-escaped, so allow for its expansion.
const MAX_BODY_RESPONSE_BYTES = 2 * MAX_HTML_BYTES + 64 * 1024;
const MESSAGE_FIELDS = "id,internetMessageId,conversationId,subject,from,sender,toRecipients,ccRecipients,replyTo,receivedDateTime,sentDateTime,hasAttachments,internetMessageHeaders";

export async function invoke(context, args) {
  const token = accessToken(context);
  const mailbox = encodeURIComponent(args.mailbox);
  const message = encodeURIComponent(args.messageId);
  const base = `${GRAPH}/users/${mailbox}/messages/${message}`;
  switch (args.operation) {
    case "get_message": return getMessage(context, token, base);
    case "list_attachments": return listAttachments(context, token, base);
    case "get_attachment": {
      if (!args.attachmentId) throw toolError("invalid_arguments", "get_attachment requires attachmentId");
      return getAttachment(context, token, base, encodeURIComponent(args.attachmentId));
    }
  }
  throw toolError("invalid_arguments", "unsupported operation");
}

async function getMessage(context, token, base) {
  const meta = await graphJSON(context, token, `${base}?$select=${MESSAGE_FIELDS}`);
  const text = await graphJSON(context, token, `${base}?$select=body`, { Prefer: 'outlook.body-content-type="text"' }, MAX_BODY_RESPONSE_BYTES);
  const html = await graphJSON(context, token, `${base}?$select=body`, { Prefer: 'outlook.body-content-type="html"' }, MAX_BODY_RESPONSE_BYTES);
  const headers = Object.fromEntries((meta.internetMessageHeaders ?? [])
    .filter((h) => ["in-reply-to", "references"].includes(String(h.name).toLowerCase()))
    .map((h) => [String(h.name).toLowerCase(), String(h.value)]));
  const bodyText = truncate(String(text.body?.content ?? ""), MAX_TEXT_BYTES);
  const artifactRefs = [];
  let bodyHtml = null;
  const htmlContent = String(html.body?.content ?? "");
  if (html.body?.contentType === "html" && htmlContent) {
    const bytes = Buffer.from(htmlContent, "utf8");
    if (bytes.byteLength > MAX_HTML_BYTES) throw toolError("message_too_large", "message HTML body exceeds the read limit");
    const ref = await writeArtifact(context, "text/html", bytes);
    artifactRefs.push(ref);
    bodyHtml = { artifactId: ref.artifactId, sizeBytes: bytes.byteLength };
  }
  return {
    result: {
      id: meta.id,
      internetMessageId: meta.internetMessageId ?? "",
      conversationId: meta.conversationId ?? "",
      inReplyTo: headers["in-reply-to"]?.trim() ?? "",
      references: (headers.references ?? "").split(/\s+/).filter(Boolean),
      subject: meta.subject ?? "",
      from: address(meta.from),
      sender: address(meta.sender),
      to: (meta.toRecipients ?? []).map(address),
      cc: (meta.ccRecipients ?? []).map(address),
      replyTo: (meta.replyTo ?? []).map(address),
      receivedAt: meta.receivedDateTime ?? null,
      sentAt: meta.sentDateTime ?? null,
      hasAttachments: Boolean(meta.hasAttachments),
      bodyText: bodyText.text,
      bodyTextTruncated: bodyText.truncated,
      bodyHtml,
    },
    artifactRefs,
  };
}

async function listAttachments(context, token, base) {
  const page = await graphJSON(context, token, `${base}/attachments?$select=id,name,contentType,size,isInline&$top=${MAX_ATTACHMENTS}`);
  const attachments = (page.value ?? []).map((a) => ({
    id: a.id,
    name: a.name ?? "",
    contentType: a.contentType ?? "application/octet-stream",
    sizeBytes: a.size ?? 0,
    isInline: Boolean(a.isInline),
    kind: kind(a["@odata.type"]),
  }));
  return { result: { attachments, truncated: Boolean(page["@odata.nextLink"]) } };
}

async function getAttachment(context, token, base, attachment) {
  const meta = await graphJSON(context, token, `${base}/attachments/${attachment}?$select=id,name,contentType,size,isInline`);
  if ((meta.size ?? 0) > MAX_ATTACHMENT_BYTES) throw toolError("attachment_too_large", "attachment exceeds the read limit");
  const kindName = kind(meta["@odata.type"]);
  if (kindName === "reference") throw toolError("unsupported_attachment", "linked cloud attachments cannot be read");
  // $value returns file bytes, or MIME for an attached message.
  const response = await graph(context, token, `${base}/attachments/${attachment}/$value`);
  const bytes = await boundedBytes(response, MAX_ATTACHMENT_BYTES, "attachment_too_large", "attachment exceeds the read limit");
  const mimeType = kindName === "item" ? "message/rfc822" : safeMime(meta.contentType);
  const ref = await writeArtifact(context, mimeType, bytes);
  return {
    result: { id: meta.id, name: meta.name ?? "", contentType: mimeType, sizeBytes: bytes.byteLength, isInline: Boolean(meta.isInline), artifactId: ref.artifactId },
    artifactRefs: [ref],
  };
}

function accessToken(context) {
  const credential = context.credentials?.microsoft365;
  if (!credential || typeof credential.accessToken !== "string" || !credential.accessToken) {
    throw toolError("credential_unavailable", "the Microsoft 365 credential is not bound");
  }
  return credential.accessToken;
}

// Immutable ids match the ids mailbox intake records.
async function graph(context, token, url, { Prefer: prefer } = {}) {
  let response;
  try {
    response = await fetch(url, {
      headers: { Authorization: `Bearer ${token}`, Accept: "application/json", Prefer: prefer ? `IdType="ImmutableId", ${prefer}` : 'IdType="ImmutableId"' },
      redirect: "manual",
      signal: context.signal,
    });
  } catch {
    throw toolError("provider_unavailable", "Microsoft 365 could not be reached", true);
  }
  if (response.ok) return response;
  await response.body?.cancel();
  throw statusError(response.status);
}

async function graphJSON(context, token, url, headers, limit = MAX_JSON_BYTES) {
  const response = await graph(context, token, url, headers);
  const bytes = await boundedBytes(response, limit, "message_too_large", "the Microsoft 365 response exceeds the read limit");
  try {
    return JSON.parse(bytes.toString("utf8"));
  } catch {
    throw toolError("provider_error", "Microsoft 365 returned an unreadable response");
  }
}

// Only the status is surfaced: provider text may echo request content.
function statusError(status) {
  if (status === 401) return toolError("credential_rejected", "Microsoft 365 rejected the access token");
  if (status === 403) return toolError("access_denied", "Microsoft 365 denied access to this mailbox");
  if (status === 404) return toolError("not_found", "the message or attachment was not found");
  if (status === 429 || status >= 500) return toolError("provider_unavailable", `Microsoft 365 returned HTTP ${status}`, true);
  return toolError("provider_error", `Microsoft 365 returned HTTP ${status}`);
}

// Reads at most limit bytes; a larger response is cancelled, never buffered.
async function boundedBytes(response, limit, code, message) {
  const declared = Number(response.headers.get("content-length"));
  if (Number.isFinite(declared) && declared > limit) {
    await response.body?.cancel();
    throw toolError(code, message);
  }
  if (!response.body) return Buffer.alloc(0);
  const reader = response.body.getReader();
  const chunks = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > limit) {
      await reader.cancel();
      throw toolError(code, message);
    }
    chunks.push(value);
  }
  return Buffer.concat(chunks);
}

async function writeArtifact(context, mimeType, bytes) {
  async function* body() { yield new Uint8Array(bytes); }
  return context.artifacts.write({ mimeType, bytes: body(), expectedSizeBytes: BigInt(bytes.byteLength) });
}

function truncate(text, limit) {
  const bytes = Buffer.from(text, "utf8");
  if (bytes.byteLength <= limit) return { text, truncated: false };
  return { text: bytes.subarray(0, limit).toString("utf8").replace(/�$/, ""), truncated: true };
}

function address(recipient) {
  return { name: recipient?.emailAddress?.name ?? "", address: recipient?.emailAddress?.address ?? "" };
}

function kind(odataType) {
  if (odataType === "#microsoft.graph.itemAttachment") return "item";
  if (odataType === "#microsoft.graph.referenceAttachment") return "reference";
  return "file";
}

function safeMime(value) {
  return typeof value === "string" && /^[a-z0-9][a-z0-9!#$&^_.+-]*\/[a-z0-9][a-z0-9!#$&^_.+-]*$/i.test(value) ? value.toLowerCase() : "application/octet-stream";
}

function toolError(code, message, retryable = false) {
  const error = new Error(message);
  error.name = "ToolError";
  error.code = code;
  error.retryable = retryable;
  return error;
}
