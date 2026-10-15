// Send one email as a bound Microsoft 365 mailbox (HOR-358, DES-HOR-358-05).
// The gateway has already limited `mailbox` to an active mailbox on this
// tool's credential slot, rendered the exact consequence summary and recorded
// the at-most-once invocation. Confirmation and pre-approval policy belong to
// HOR-599.
//
// Draft-then-send: Graph's sendMail returns no identifiers and Message-ID
// cannot be set, so the draft is created first and its internetMessageId and
// conversationId are known before the send. A send whose outcome is unknown
// returns `outcome_unknown` with the draft identity, never a silent retry.
export const identity = { name: "platform.mailbox.send", version: "1.0.0" };

const GRAPH = "https://graph.microsoft.com/v1.0";
// Provider responses are read through this byte limit before parsing.
const MAX_JSON_BYTES = 1024 * 1024;
// Identifiers missing from the draft are re-read a bounded number of times.
const ID_READ_ATTEMPTS = 3;
const ID_READ_DELAY_MS = 500;

export async function invoke(context, args) {
  const token = accessToken(context);
  const mailbox = `${GRAPH}/users/${encodeURIComponent(args.mailbox)}`;
  const message = {
    subject: args.subject,
    body: { contentType: args.bodyType === "html" ? "HTML" : "Text", content: args.body },
    toRecipients: [{ emailAddress: { address: args.to } }],
  };

  // 1. Draft. Nothing has been sent if this fails.
  let draft;
  try {
    draft = args.replyToMessageId
      ? await graphJSON(context, token, "POST", `${mailbox}/messages/${encodeURIComponent(args.replyToMessageId)}/createReply`, { message })
      : await graphJSON(context, token, "POST", `${mailbox}/messages`, message);
  } catch (error) {
    throw notSent(error);
  }
  if (!draft?.id) throw toolError("not_sent", "Microsoft 365 returned no draft; nothing was sent");
  // Both identifiers must be known before the send (DES-HOR-358-05); without
  // them a reply could not be matched, so the draft is never sent.
  for (let attempt = 0; attempt < ID_READ_ATTEMPTS && !(draft.internetMessageId && draft.conversationId); attempt++) {
    if (attempt > 0) await delay(ID_READ_DELAY_MS, context.signal);
    try {
      draft = { ...draft, ...await graphJSON(context, token, "GET", `${mailbox}/messages/${encodeURIComponent(draft.id)}?$select=id,internetMessageId,conversationId`) };
    } catch (error) {
      throw notSent(error);
    }
  }
  if (!draft.internetMessageId || !draft.conversationId) {
    throw toolError("not_sent", "Microsoft 365 did not return the draft's identifiers; nothing was sent", true, { providerMessageId: draft.id });
  }
  const sent = {
    providerMessageId: draft.id,
    internetMessageId: draft.internetMessageId,
    conversationId: draft.conversationId,
  };

  // 2. Send. A definite rejection means not sent; anything else is unknown.
  let response;
  try {
    response = await fetch(`${mailbox}/messages/${encodeURIComponent(draft.id)}/send`, {
      method: "POST", headers: headers(token), redirect: "manual", signal: context.signal,
    });
  } catch {
    throw toolError("outcome_unknown", "the send request did not complete; the email may or may not have been sent", false, sent);
  }
  await response.body?.cancel();
  if (response.status === 202 || response.ok) return { result: { status: "sent", ...sent } };
  if (response.status >= 400 && response.status < 500 && response.status !== 408 && response.status !== 429) {
    throw toolError("not_sent", `Microsoft 365 rejected the send (HTTP ${response.status}); the draft was not sent`, false, sent);
  }
  throw toolError("outcome_unknown", `Microsoft 365 returned HTTP ${response.status}; the email may or may not have been sent`, false, sent);
}

function accessToken(context) {
  const credential = context.credentials?.microsoft365;
  if (!credential || typeof credential.accessToken !== "string" || !credential.accessToken) {
    throw toolError("credential_unavailable", "the Microsoft 365 credential is not bound");
  }
  return credential.accessToken;
}

// Immutable ids match the ids mailbox intake records.
function headers(token) {
  return { Authorization: `Bearer ${token}`, Accept: "application/json", "Content-Type": "application/json", Prefer: 'IdType="ImmutableId"' };
}

async function graphJSON(context, token, method, url, body) {
  let response;
  try {
    response = await fetch(url, {
      method, headers: headers(token), body: body === undefined ? undefined : JSON.stringify(body), redirect: "manual", signal: context.signal,
    });
  } catch {
    throw toolError("provider_unavailable", "Microsoft 365 could not be reached", true);
  }
  if (!response.ok) {
    await response.body?.cancel();
    throw statusError(response.status);
  }
  return boundedJSON(response, MAX_JSON_BYTES);
}

// Reads at most limit bytes before parsing; a larger response is cancelled.
async function boundedJSON(response, limit) {
  const declared = Number(response.headers.get("content-length"));
  if (Number.isFinite(declared) && declared > limit) {
    await response.body?.cancel();
    throw toolError("provider_error", "the Microsoft 365 response exceeds the read limit");
  }
  const chunks = [];
  let total = 0;
  if (response.body) {
    const reader = response.body.getReader();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > limit) {
        await reader.cancel();
        throw toolError("provider_error", "the Microsoft 365 response exceeds the read limit");
      }
      chunks.push(value);
    }
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    throw toolError("provider_error", "Microsoft 365 returned an unreadable response");
  }
}

function delay(ms, signal) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener("abort", () => { clearTimeout(timer); reject(toolError("canceled", "tool invocation canceled")); }, { once: true });
  });
}

function notSent(error) {
  if (error?.name === "ToolError") {
    error.message = `${error.message}; nothing was sent`;
    return error;
  }
  return toolError("not_sent", "the draft could not be created; nothing was sent");
}

// Only the status is surfaced: provider text may echo request content.
function statusError(status) {
  if (status === 401) return toolError("credential_rejected", "Microsoft 365 rejected the access token");
  if (status === 403) return toolError("access_denied", "Microsoft 365 denied access to this mailbox");
  if (status === 404) return toolError("not_found", "the message to reply to was not found");
  if (status === 429 || status >= 500) return toolError("provider_unavailable", `Microsoft 365 returned HTTP ${status}`, true);
  return toolError("provider_error", `Microsoft 365 returned HTTP ${status}`);
}

function toolError(code, message, retryable = false, details) {
  const error = new Error(message);
  error.name = "ToolError";
  error.code = code;
  error.retryable = retryable;
  if (details !== undefined) error.details = details;
  return error;
}
