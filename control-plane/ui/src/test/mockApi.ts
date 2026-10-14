// An in-memory customer API over the fixtures, for component tests and the
// local design preview. It mirrors the server's projections closely enough to
// exercise every screen; it is never shipped.
import type { AuthProfile } from "../auth/authApi";
import type { WorkItem } from "../lib/types";
import * as fx from "./fixtures";

export interface MockCall {
  method: string;
  path: string;
  body?: unknown;
  headers: Record<string, string>;
}

export interface MockApi {
  handle: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
  calls: MockCall[];
  items: WorkItem[];
  /** Pushes one work event to every open stream. */
  emit: (code: string, workItemId: string) => void;
  /** Ends every open stream with an error (a dropped connection). */
  drop: () => void;
  profile: AuthProfile;
}

export const operator: AuthProfile = {
  id: "identity-2",
  email: "rita.mendes@example.com",
  displayName: "Rita Mendes",
  role: "operator",
  locale: "en",
  localePreference: "en",
  organizationLocale: "",
  updatedAt: "2026-10-01T10:00:00Z",
};

export const admin: AuthProfile = {
  ...operator,
  id: "identity-1",
  email: "nadia@example.com",
  displayName: "Nadia Costa",
  role: "admin",
};

function json(data: unknown, status = 200): Response {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

export function createMockApi(
  profile: AuthProfile = operator,
  overrides: Partial<{
    eventsStatus: number;
    workflowsStatus: number;
    /** Starts signed out; any sign-in succeeds (design preview of sign-in). */
    anonymous: boolean;
  }> = {},
): MockApi {
  const items = fx.items.map((item) => ({ ...item }));
  const calls: MockCall[] = [];
  const streams = new Set<ReadableStreamDefaultController<Uint8Array>>();
  let cursor = 100;
  let signedIn = !overrides.anonymous;
  const encoder = new TextEncoder();

  function emit(code: string, workItemId: string) {
    cursor += 1;
    const frame = `id: ${cursor}\nevent: ${code}\ndata: ${JSON.stringify({ workItemId, code })}\n\n`;
    for (const controller of streams) controller.enqueue(encoder.encode(frame));
  }

  function drop() {
    for (const controller of streams)
      controller.error(new TypeError("network"));
    streams.clear();
  }

  function list(params: URLSearchParams): WorkItem[] {
    const state = params.get("state");
    const key = params.get("workflowKey");
    const search = params.get("search")?.toLowerCase();
    const from = params.get("from");
    return items.filter((item) => {
      if (state && item.state !== state) return false;
      if (key && item.workflowKey !== key) return false;
      if (
        search &&
        !`${item.title} ${item.source.title}`.toLowerCase().includes(search)
      )
        return false;
      if (from && (item.state === "done" || item.state === "failed"))
        return !!item.finishedAt && item.finishedAt >= from;
      return true;
    });
  }

  async function handle(
    input: RequestInfo | URL,
    init: RequestInit = {},
  ): Promise<Response> {
    const url = new URL(String(input), "http://localhost");
    const method = (init.method ?? "GET").toUpperCase();
    const headers = Object.fromEntries(new Headers(init.headers).entries());
    let body: unknown;
    if (typeof init.body === "string") {
      try {
        body = JSON.parse(init.body);
      } catch {
        body = init.body;
      }
    }
    calls.push({ method, path: url.pathname + url.search, body, headers });
    const path = url.pathname;
    let match: RegExpMatchArray | null;

    if (path === "/v1/auth/sign-in" && method === "POST") {
      signedIn = true;
      return json({ profile, csrfToken: "csrf-1" });
    }
    if (path === "/v1/auth/session" && !signedIn)
      return json({ authenticated: false, locale: profile.locale });
    if (path === "/v1/auth/session")
      return json({
        authenticated: true,
        profile,
        csrfToken: "csrf-1",
        locale: profile.locale,
      });
    if (path === "/v1/profile") return json({ profile, csrfToken: "csrf-2" });
    if (path === "/v1/sessions") return json([]);
    if (path === "/v1/auth/sign-out")
      return new Response(null, { status: 204 });
    if (path === "/v1/workflows")
      return overrides.workflowsStatus
        ? json({ error: "unavailable" }, overrides.workflowsStatus)
        : json({ workflows: fx.workflows });
    if (path === "/v1/work-dashboard") return json(fx.summary);
    if (path === "/v1/work-events") {
      if (overrides.eventsStatus)
        return json({ error: "unauthorized" }, overrides.eventsStatus);
      const signal = init.signal;
      const stream = new ReadableStream<Uint8Array>({
        start(controller) {
          streams.add(controller);
          signal?.addEventListener("abort", () => {
            streams.delete(controller);
            try {
              controller.error(new DOMException("aborted", "AbortError"));
            } catch {
              /* already closed */
            }
          });
        },
      });
      return new Response(stream, {
        status: 200,
        headers: { "Content-Type": "text/event-stream" },
      });
    }
    if (path === "/v1/work-items" && method === "GET")
      return json(list(url.searchParams));
    if ((match = path.match(/^\/v1\/work-items\/([^/]+)(\/[a-z]+)?$/))) {
      const id = decodeURIComponent(match[1]);
      const found = items.find((candidate) => candidate.id === id);
      if (!found) return json({ error: "work item not found" }, 404);
      switch (match[2]) {
        case undefined:
          return json(found);
        case "/attempts":
          return json([
            {
              id: found.currentAttemptId,
              workItemId: found.id,
              number: 1,
              definitionKey: found.workflowKey,
              definitionVersion: "1",
              createdAt: found.createdAt,
            },
          ]);
        case "/timeline":
          return json(fx.timelineFor(found.id));
        case "/blocker":
          return found.state === "blocked" && fx.blockers[found.id]
            ? json(fx.blockers[found.id])
            : json({ error: "no open blocker" }, 404);
        case "/feedback":
        case "/consequences":
        case "/artifacts":
          return json([]);
      }
    }
    if ((match = path.match(/^\/v1\/work-attempts\/([^/]+)\/nodes$/))) {
      const attempt = decodeURIComponent(match[1]);
      const found = items.find(
        (candidate) => candidate.currentAttemptId === attempt,
      );
      return json(found ? fx.nodesFor(found.id) : []);
    }
    if (
      (match = path.match(/^\/v1\/work-blockers\/([^/]+)\/responses$/)) &&
      method === "POST"
    ) {
      const blocker = Object.values(fx.blockers).find(
        (candidate) => candidate.id === match![1],
      );
      const found =
        blocker &&
        items.find((candidate) => candidate.id === blocker.workItemId);
      if (!found || found.state !== "blocked")
        return json({ error: "blocker resolved" }, 409);
      found.state = "in_progress";
      found.blocker = undefined;
      found.updatedAt = new Date().toISOString();
      emit("blocker_resolved", found.id);
      return json({ ...blocker, state: "resolved" });
    }
    return json({ error: "not found" }, 404);
  }

  return { handle, calls, items, emit, drop, profile };
}
