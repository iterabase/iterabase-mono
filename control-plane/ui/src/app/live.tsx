import {
  createContext,
  DependencyList,
  ReactNode,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
} from "react";
import { APIError, listWorkflows } from "../lib/api";
import { streamWorkEvents, type Connection } from "../lib/events";
import type { WorkflowSummary } from "../lib/types";
import { isForbidden, useSession } from "./session";

// One work-event stream for the whole app. Screens never apply events to
// their own state: an event bumps `version`, and every live query re-reads its
// projection from the server. Bursts (including the replay after a reconnect)
// collapse into one refresh.

interface Live {
  connection: Connection;
  version: number;
  workflows: WorkflowSummary[] | null;
  workflowsError: boolean;
  reloadWorkflows: () => void;
}

const LiveContext = createContext<Live | null>(null);

export function useLive(): Live {
  const live = useContext(LiveContext);
  if (!live) throw new Error("useLive outside LiveProvider");
  return live;
}

const SETTLE_MS = 250;

export function LiveProvider({ children }: { children: ReactNode }) {
  const session = useSession();
  const [connection, setConnection] = useState<Connection>("connecting");
  const [version, setVersion] = useState(0);
  const [workflows, setWorkflows] = useState<WorkflowSummary[] | null>(null);
  const [workflowsError, setWorkflowsError] = useState(false);
  const lostRef = useRef(session.lost);
  lostRef.current = session.lost;

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const stop = streamWorkEvents({
      onEvent: () => {
        clearTimeout(timer);
        timer = setTimeout(() => setVersion((v) => v + 1), SETTLE_MS);
      },
      onConnection: (state) => {
        setConnection((previous) => {
          // Coming back from a gap: re-read everything once.
          if (state === "live" && previous === "reconnecting")
            setVersion((v) => v + 1);
          return state;
        });
      },
      onSessionLost: () => lostRef.current(new APIError(401, "")),
    });
    return () => {
      clearTimeout(timer);
      stop();
    };
  }, []);

  const loadWorkflows = useCallback(async () => {
    setWorkflowsError(false);
    try {
      setWorkflows(await listWorkflows());
    } catch (err) {
      if (!lostRef.current(err)) setWorkflowsError(true);
    }
  }, []);

  useEffect(() => {
    void loadWorkflows();
  }, [loadWorkflows]);

  return (
    <LiveContext.Provider
      value={{
        connection,
        version,
        workflows,
        workflowsError,
        reloadWorkflows: () => void loadWorkflows(),
      }}
    >
      {children}
    </LiveContext.Provider>
  );
}

export type QueryState<T> =
  | { status: "loading"; data?: undefined }
  | { status: "ready"; data: T; refreshing: boolean; stale: boolean }
  | { status: "error"; data?: undefined; forbidden: boolean };

/**
 * Loads a projection and re-reads it on every live change. A failed refresh
 * keeps the last data but marks it stale, so a screen can say it is out of
 * date instead of pretending.
 */
export function useLiveQuery<T>(
  load: () => Promise<T>,
  deps: DependencyList,
  { live = true }: { live?: boolean } = {},
): QueryState<T> & { reload: () => void } {
  const session = useSession();
  const { version } = useLive();
  const [state, setState] = useState<QueryState<T>>({ status: "loading" });
  const [nonce, setNonce] = useState(0);
  const loadRef = useRef(load);
  loadRef.current = load;
  const lostRef = useRef(session.lost);
  lostRef.current = session.lost;
  const recheckRef = useRef(session.recheck);
  recheckRef.current = session.recheck;
  const liveVersion = live ? version : 0;

  // A dependency change is a new question: show loading, never old answers.
  useEffect(() => {
    setState({ status: "loading" });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  useEffect(() => {
    let cancelled = false;
    setState((current) =>
      current.status === "ready" ? { ...current, refreshing: true } : current,
    );
    loadRef
      .current()
      .then((data) => {
        if (!cancelled)
          setState({ status: "ready", data, refreshing: false, stale: false });
      })
      .catch((err: unknown) => {
        if (cancelled || lostRef.current(err)) return;
        const forbidden = isForbidden(err);
        if (forbidden) void recheckRef.current();
        setState((current) =>
          current.status === "ready" && !forbidden
            ? { ...current, refreshing: false, stale: true }
            : { status: "error", forbidden },
        );
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, liveVersion, nonce]);

  return { ...state, reload: () => setNonce((n) => n + 1) } as QueryState<T> & {
    reload: () => void;
  };
}
