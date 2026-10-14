import {
  createContext,
  ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { setLocale as setActiveLocale, type Locale } from "../i18n";
import { APIError } from "../lib/api";
import { AuthAPIError, authApi, type AuthProfile } from "../auth/authApi";

// The signed-in browser session (HOR-582). Every screen reads the person, the
// CSRF proof and the session-loss path from here. Authority is re-read from
// the server: a 401 ends the session and a 403 refreshes the profile, so
// Admin-only UI unmounts as soon as the role is gone.

export interface Session {
  profile: AuthProfile;
  locale: Locale;
  isAdmin: boolean;
  /** Runs an unsafe request with the CSRF proof, refreshing a stale proof once. */
  withCSRF: <T>(action: (csrf: string) => Promise<T>) => Promise<T>;
  /** Handles authority errors. Returns true when the error was a lost session. */
  lost: (err: unknown) => boolean;
  /** Re-reads the profile after a refusal; returns the fresh role. */
  recheck: () => Promise<void>;
  updateProfile: (profile: AuthProfile) => void;
  expire: (message?: string) => void;
  signOut: () => Promise<boolean>;
}

const SessionContext = createContext<Session | null>(null);

export function useSession(): Session {
  const session = useContext(SessionContext);
  if (!session) throw new Error("useSession outside SessionProvider");
  return session;
}

function status(err: unknown): number {
  if (err instanceof APIError || err instanceof AuthAPIError) return err.status;
  return 0;
}

function code(err: unknown): string {
  if (err instanceof APIError || err instanceof AuthAPIError) return err.code;
  return "";
}

export function isForbidden(err: unknown): boolean {
  return status(err) === 403;
}

export function isNotFound(err: unknown): boolean {
  return status(err) === 404;
}

export function SessionProvider({
  profile: initialProfile,
  csrfToken,
  onSessionLost,
  children,
}: {
  profile: AuthProfile;
  csrfToken: string;
  /** Reports a lost session with the language the sign-in page should use. */
  onSessionLost: (message: string | undefined, locale: Locale) => void;
  children: ReactNode;
}) {
  const [profile, setProfile] = useState(initialProfile);
  // The proof rotates on refresh; a ref keeps refreshes from re-rendering
  // (and re-fetching) every screen.
  const csrf = useRef(csrfToken);
  const locale = profile.locale;
  useEffect(() => setActiveLocale(locale), [locale]);

  const onLostRef = useRef(onSessionLost);
  onLostRef.current = onSessionLost;
  const expired = useRef(false);
  const expire = useCallback(
    (message?: string) => {
      if (expired.current) return;
      expired.current = true;
      onLostRef.current(message, locale);
    },
    [locale],
  );

  const lost = useCallback(
    (err: unknown) => {
      if (status(err) === 401 || code(err) === "session_expired") {
        expire("sessionExpired");
        return true;
      }
      return false;
    },
    [expire],
  );

  const recheck = useCallback(async () => {
    try {
      const refreshed = await authApi.profile();
      csrf.current = refreshed.csrfToken;
      setProfile(refreshed.profile);
    } catch (err) {
      lost(err);
    }
  }, [lost]);

  const withCSRF = useCallback(
    async <T,>(action: (token: string) => Promise<T>): Promise<T> => {
      try {
        return await action(csrf.current);
      } catch (err) {
        if (code(err) !== "csrf") throw err;
        const refreshed = await authApi.profile();
        csrf.current = refreshed.csrfToken;
        setProfile(refreshed.profile);
        return action(refreshed.csrfToken);
      }
    },
    [],
  );

  const signOut = useCallback(async () => {
    try {
      await authApi.signOutCurrent(csrf.current);
    } catch {
      // The server session may still be valid: stay signed in and say so.
      return false;
    }
    expire();
    return true;
  }, [expire]);

  const value = useMemo<Session>(
    () => ({
      profile,
      locale,
      isAdmin: profile.role === "admin",
      withCSRF,
      lost,
      recheck,
      updateProfile: setProfile,
      expire,
      signOut,
    }),
    [profile, locale, withCSRF, lost, recheck, expire, signOut],
  );

  return (
    <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
  );
}
