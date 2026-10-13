import type {
  Blocker,
  Consequence,
  DashboardSummary,
  DetailData,
  Feedback,
  NodeExecution,
  Attempt,
  TimelineEvent,
  WorkArtifact,
  WorkItem,
} from "./types";

export class APIError extends Error {
  constructor(
    public status: number,
    message: string,
    public code = "",
  ) {
    super(message);
  }
}

// Every work call rides the same-origin browser session cookie. Unsafe methods
// also carry the session-bound CSRF proof.
async function request<T>(
  path: string,
  init: RequestInit = {},
  csrf?: string,
): Promise<T> {
  const response = await fetch(path, {
    ...init,
    credentials: "same-origin",
    headers: {
      ...(init.body && !(init.body instanceof Blob)
        ? { "Content-Type": "application/json" }
        : {}),
      ...(csrf ? { "X-CSRF-Token": csrf } : {}),
      ...init.headers,
    },
  });
  if (!response.ok) {
    let message = response.statusText;
    let code = "";
    try {
      const payload = (await response.json()) as {
        error?: string;
        code?: string;
      };
      message = payload.error || message;
      code = payload.code || code;
    } catch {
      /* empty response */
    }
    throw new APIError(response.status, message, code);
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export interface Period {
  from: Date;
  to: Date;
}
const periodParams = (period: Period) =>
  new URLSearchParams({
    from: period.from.toISOString(),
    to: period.to.toISOString(),
  });

export async function loadDashboard(
  period: Period,
  search = "",
): Promise<{ summary: DashboardSummary; items: WorkItem[] }> {
  const summaryParams = periodParams(period);
  const itemParams = periodParams(period);
  itemParams.set("limit", "200");
  if (search.trim()) itemParams.set("search", search.trim());
  const [summary, items] = await Promise.all([
    request<DashboardSummary>(`/v1/work-dashboard?${summaryParams}`),
    request<WorkItem[]>(`/v1/work-items?${itemParams}`),
  ]);
  return { summary, items };
}

async function optional<T>(promise: Promise<T>): Promise<T | null> {
  try {
    return await promise;
  } catch (error) {
    if (error instanceof APIError && error.status === 404) return null;
    throw error;
  }
}

export async function loadDetail(item: WorkItem): Promise<DetailData> {
  const id = encodeURIComponent(item.id);
  const [
    fresh,
    attempts,
    timeline,
    blocker,
    feedback,
    consequences,
    artifacts,
  ] = await Promise.all([
    request<WorkItem>(`/v1/work-items/${id}`),
    request<Attempt[]>(`/v1/work-items/${id}/attempts`),
    request<TimelineEvent[]>(`/v1/work-items/${id}/timeline?limit=1000`),
    optional(request<Blocker>(`/v1/work-items/${id}/blocker`)),
    request<Feedback[]>(`/v1/work-items/${id}/feedback`),
    request<Consequence[]>(`/v1/work-items/${id}/consequences`),
    request<WorkArtifact[]>(`/v1/work-items/${id}/artifacts`),
  ]);
  const nodeGroups = await Promise.all(
    attempts.map((attempt) =>
      request<NodeExecution[]>(
        `/v1/work-attempts/${encodeURIComponent(attempt.id)}/nodes`,
      ),
    ),
  );
  return {
    item: fresh,
    attempts,
    timeline,
    blocker,
    feedback,
    consequences,
    artifacts,
    nodes: nodeGroups.flat(),
  };
}

export async function respondToBlocker(
  csrf: string,
  blocker: Blocker,
  outcome: string,
  response: Record<string, unknown>,
  artifactRefs: Array<{ artifactId: string; metadata?: unknown }> = [],
  confirmedInvocationIds: string[] = [],
): Promise<void> {
  await request(
    `/v1/work-blockers/${encodeURIComponent(blocker.id)}/responses`,
    {
      method: "POST",
      body: JSON.stringify({
        outcome,
        response,
        artifactRefs,
        confirmedInvocationIds:
          blocker.kind === "consequence_confirmation"
            ? confirmedInvocationIds
            : undefined,
      }),
    },
    csrf,
  );
}

export async function stopWaiting(csrf: string, item: WorkItem): Promise<void> {
  await request(
    `/v1/work-items/${encodeURIComponent(item.id)}/wait/stop`,
    { method: "POST" },
    csrf,
  );
}

export function saveFeedback(
  csrf: string,
  item: WorkItem,
  category: string,
  explanation: string,
): Promise<Feedback> {
  return request(
    `/v1/work-items/${encodeURIComponent(item.id)}/feedback`,
    {
      method: "POST",
      body: JSON.stringify({
        attemptId: item.currentAttemptId,
        category,
        explanation: explanation || undefined,
      }),
    },
    csrf,
  );
}

export function createRevision(
  csrf: string,
  item: WorkItem,
  feedbackId: string,
  guidance: string,
  consequences: Consequence[],
): Promise<WorkItem> {
  return request(
    `/v1/work-items/${encodeURIComponent(item.id)}/revisions`,
    {
      method: "POST",
      body: JSON.stringify({
        feedbackId,
        actionableGuidance: guidance,
        confirmedInvocationIds: consequences.map((c) => c.invocationId),
      }),
    },
    csrf,
  );
}

export async function uploadArtifact(
  csrf: string,
  file: File,
): Promise<{ artifactId: string }> {
  const artifact = await request<{ artifactId?: unknown }>(
    "/v1/artifacts",
    {
      method: "POST",
      headers: {
        "Content-Type": file.type || "application/octet-stream",
        "X-Artifact-Source": file.name,
      },
      body: file,
    },
    csrf,
  );
  if (typeof artifact.artifactId !== "string" || !artifact.artifactId.trim()) {
    throw new APIError(502, "Artifact upload response was incomplete.");
  }
  return { artifactId: artifact.artifactId };
}

export async function downloadArtifact(artifact: WorkArtifact): Promise<void> {
  const response = await fetch(
    `/v1/artifacts/${encodeURIComponent(artifact.artifactId)}`,
    { credentials: "same-origin" },
  );
  if (!response.ok) throw new APIError(response.status, response.statusText);
  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download =
    typeof artifact.metadata?.name === "string"
      ? artifact.metadata.name
      : `artifact-${artifact.artifactId}`;
  link.click();
  URL.revokeObjectURL(url);
}

// This fetch-based SSE reader rides the session cookie and resumes with
// Last-Event-ID, keeping credentials out of URLs and browser storage. A lost
// session stops the reader and is reported instead of retried.
export function subscribe(
  onEvent: () => void,
  onSessionLost: () => void,
): () => void {
  const controller = new AbortController();
  let cursor = "";
  const run = async () => {
    while (!controller.signal.aborted) {
      try {
        const response = await fetch("/v1/work-events", {
          credentials: "same-origin",
          headers: cursor ? { "Last-Event-ID": cursor } : {},
          signal: controller.signal,
        });
        if (response.status === 401) {
          onSessionLost();
          return;
        }
        if (!response.ok || !response.body)
          throw new APIError(response.status, response.statusText);
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        while (!controller.signal.aborted) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let boundary = buffer.indexOf("\n\n");
          while (boundary >= 0) {
            const frame = buffer.slice(0, boundary);
            buffer = buffer.slice(boundary + 2);
            const id = frame
              .split("\n")
              .find((line) => line.startsWith("id: "))
              ?.slice(4);
            if (id) {
              cursor = id;
              onEvent();
            }
            boundary = buffer.indexOf("\n\n");
          }
        }
      } catch (error) {
        if (controller.signal.aborted) return;
        await new Promise((resolve) => setTimeout(resolve, 1500));
      }
    }
  };
  void run();
  return () => controller.abort();
}
