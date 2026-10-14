// Live work events (GET /v1/work-events). A fetch-based SSE reader rides the
// session cookie and resumes with Last-Event-ID, keeping credentials out of
// URLs and browser storage. The stream only says that something changed: the
// app then re-reads the projections it shows, so the screen never shows
// progress the server has not recorded.

export type Connection = "connecting" | "live" | "reconnecting";

export interface WorkEvent {
  cursor: string;
  code: string;
  workItemId?: string;
}

export interface StreamHandlers {
  onEvent: (event: WorkEvent) => void;
  onConnection: (state: Connection) => void;
  onSessionLost: () => void;
}

/** Reconnect delays grow to 15 s so an outage never hammers the server. */
export function reconnectDelay(failures: number): number {
  return Math.min(15_000, 1_000 * 2 ** Math.max(0, failures - 1));
}

export function parseFrame(frame: string): WorkEvent | null {
  let cursor = "";
  let code = "";
  let data = "";
  for (const line of frame.split("\n")) {
    if (line.startsWith("id:")) cursor = line.slice(3).trim();
    else if (line.startsWith("event:")) code = line.slice(6).trim();
    else if (line.startsWith("data:")) data += line.slice(5).trim();
  }
  if (!cursor) return null;
  let workItemId: string | undefined;
  try {
    const parsed = JSON.parse(data) as { workItemId?: unknown };
    if (typeof parsed.workItemId === "string") workItemId = parsed.workItemId;
  } catch {
    /* heartbeat or malformed data: the cursor still advances */
  }
  return { cursor, code, workItemId };
}

export function streamWorkEvents(handlers: StreamHandlers): () => void {
  const controller = new AbortController();
  let cursor = "";
  let failures = 0;
  const run = async () => {
    while (!controller.signal.aborted) {
      handlers.onConnection(failures === 0 ? "connecting" : "reconnecting");
      try {
        const response = await fetch("/v1/work-events", {
          credentials: "same-origin",
          headers: cursor ? { "Last-Event-ID": cursor } : {},
          signal: controller.signal,
        });
        if (response.status === 401) {
          handlers.onSessionLost();
          return;
        }
        if (!response.ok || !response.body)
          throw new Error(`work events: ${response.status}`);
        failures = 0;
        handlers.onConnection("live");
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        while (!controller.signal.aborted) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let boundary = buffer.indexOf("\n\n");
          while (boundary >= 0) {
            const event = parseFrame(buffer.slice(0, boundary));
            buffer = buffer.slice(boundary + 2);
            if (event) {
              cursor = event.cursor;
              handlers.onEvent(event);
            }
            boundary = buffer.indexOf("\n\n");
          }
        }
        // The server closed the stream: reconnect from the last cursor.
        failures = 1;
      } catch {
        if (controller.signal.aborted) return;
        failures += 1;
      }
      if (controller.signal.aborted) return;
      handlers.onConnection("reconnecting");
      await new Promise((resolve) =>
        setTimeout(resolve, reconnectDelay(failures)),
      );
    }
  };
  void run();
  return () => controller.abort();
}
