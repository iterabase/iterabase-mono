import { useCallback, useEffect, useRef, useState } from "react";
import { AccessRequest, AuthAPIError, authApi } from "../auth/authApi";

export interface AccessRequests {
  /** null while loading (or for non-Admins, who never load them). */
  requests: AccessRequest[] | null;
  loadFailed: boolean;
  reload: () => Promise<void>;
}

/**
 * The verified access requests waiting for an Admin: the People panel lists
 * them, the People tab and the phone More row count them. A denied or lost
 * session goes to `onError`; other failures only mark the load as failed.
 */
export function useAccessRequests(
  enabled: boolean,
  onError?: (err: unknown) => void,
): AccessRequests {
  const [requests, setRequests] = useState<AccessRequest[] | null>(null);
  const [loadFailed, setLoadFailed] = useState(false);
  const onErrorRef = useRef(onError);
  onErrorRef.current = onError;

  const reload = useCallback(async () => {
    if (!enabled) return;
    setLoadFailed(false);
    try {
      setRequests(await authApi.accessRequests());
    } catch (err) {
      if (
        err instanceof AuthAPIError &&
        (err.status === 403 || err.status === 401)
      ) {
        onErrorRef.current?.(err);
        setRequests([]);
        return;
      }
      setLoadFailed(true);
    }
  }, [enabled]);

  useEffect(() => {
    void reload();
  }, [reload]);

  return { requests, loadFailed, reload };
}
