import type {
  WorkState,
  WorkflowSummary,
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

// The customer work API. Every call rides the same-origin browser session
// cookie (HOR-582); unsafe methods also carry the session-bound CSRF proof.

export class APIError extends Error {
  constructor(
    public status: number,
    message: string,
    public code = "",
  ) {
    super(message);
  }
}

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

export interface WorkQuery {
  from?: Date;
  to?: Date;
  state?: WorkState;
  workflowKey?: string;
  search?: string;
  limit?: number;
}

/** Active items always; done and failed items finished within the period. */
export function listWorkItems(query: WorkQuery = {}): Promise<WorkItem[]> {
  const params = new URLSearchParams();
  if (query.from) params.set("from", query.from.toISOString());
  if (query.to) params.set("to", query.to.toISOString());
  if (query.state) params.set("state", query.state);
  if (query.workflowKey) params.set("workflowKey", query.workflowKey);
  if (query.search?.trim()) params.set("search", query.search.trim());
  params.set("limit", String(query.limit ?? 200));
  return request<WorkItem[]>(`/v1/work-items?${params}`);
}

export function loadSummary(period: Period): Promise<DashboardSummary> {
  const params = new URLSearchParams({
    from: period.from.toISOString(),
    to: period.to.toISOString(),
  });
  return request<DashboardSummary>(`/v1/work-dashboard?${params}`);
}

export async function listWorkflows(): Promise<WorkflowSummary[]> {
  const body = await request<{ workflows: WorkflowSummary[] }>("/v1/workflows");
  return body.workflows;
}

export function getWorkItem(id: string): Promise<WorkItem> {
  return request<WorkItem>(`/v1/work-items/${encodeURIComponent(id)}`);
}

export function listConsequences(id: string): Promise<Consequence[]> {
  return request<Consequence[]>(
    `/v1/work-items/${encodeURIComponent(id)}/consequences`,
  );
}

async function optional<T>(promise: Promise<T>): Promise<T | null> {
  try {
    return await promise;
  } catch (error) {
    if (error instanceof APIError && error.status === 404) return null;
    throw error;
  }
}

export async function loadDetail(itemId: string): Promise<DetailData> {
  const id = encodeURIComponent(itemId);
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

/**
 * Opens a PDF or screenshot in a new tab and downloads any other artifact
 * (an email or file). The bytes come through the
 * session; no artifact URL with credentials is ever exposed.
 */
export async function openArtifact(artifact: WorkArtifact): Promise<void> {
  const response = await fetch(
    `/v1/artifacts/${encodeURIComponent(artifact.artifactId)}`,
    { credentials: "same-origin" },
  );
  if (!response.ok) throw new APIError(response.status, response.statusText);
  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const viewable = /^(application\/pdf|image\/)/.test(artifact.mimeType);
  // "noopener" would make window.open return null; drop the opener instead.
  const opened = viewable ? window.open(url, "_blank") : null;
  if (opened) opened.opener = null;
  if (!opened) {
    const link = document.createElement("a");
    link.href = url;
    link.download = artifactName(artifact);
    link.click();
  }
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

export function artifactName(artifact: WorkArtifact): string {
  return typeof artifact.metadata?.name === "string"
    ? artifact.metadata.name
    : `artifact-${artifact.artifactId}`;
}
