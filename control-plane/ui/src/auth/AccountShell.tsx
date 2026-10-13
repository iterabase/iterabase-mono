import {
  FormEvent,
  ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  AccessRequest,
  AuthAPIError,
  AuthProfile,
  LocaleCode,
  OrganizationSettings,
  Role,
  SessionInfo,
  authApi,
} from "./authApi";
import type { TFunction } from "i18next";
import { fixedT, setLocale as setActiveLocale } from "../i18n";
import { formatDateTime, formatNumber } from "../i18n/format";

type SettingsT = TFunction<"settings">;

type View = "profile" | "sessions" | "requests";

interface Props {
  initialProfile: AuthProfile;
  initialCSRF: string;
  /** Reports a lost session with the language the sign-in page should use. */
  onSessionLost: (message?: string, locale?: LocaleCode) => void;
  onOpenWork?: () => void;
}

interface PendingAction {
  run: () => Promise<void>;
}

interface Confirm {
  title: string;
  body: string;
  confirmLabel: string;
  run: () => Promise<void>;
}

export default function AccountShell({
  initialProfile,
  initialCSRF,
  onSessionLost,
  onOpenWork,
}: Props) {
  const [profile, setProfile] = useState(initialProfile);
  const [csrf, setCSRF] = useState(initialCSRF);
  const [locale, setLocale] = useState<LocaleCode>(initialProfile.locale);
  const [view, setView] = useState<View>("profile");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [pending, setPending] = useState<PendingAction | null>(null);
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const invoker = useRef<HTMLElement | null>(null);
  // Stable per locale, so panels that depend on it don't reload (and drop
  // unsaved input) on unrelated shell renders.
  const c = useMemo(() => fixedT(locale, "settings"), [locale]);

  useEffect(() => setActiveLocale(locale), [locale]);

  const sessionLost = useCallback(
    (message?: string) => onSessionLost(message, locale),
    [onSessionLost, locale],
  );

  const handleError = useCallback(
    (err: unknown) => {
      const code = err instanceof AuthAPIError ? err.code : "unavailable";
      if (
        code === "session_expired" ||
        (err instanceof AuthAPIError && err.status === 401)
      ) {
        sessionLost(c("sessionExpired"));
        return;
      }
      if (code === "recent_auth_required") {
        setError("");
        return;
      }
      if (code === "forbidden") setError(c("forbidden"));
      else if (code === "throttled") setError(c("throttled"));
      else if (code === "state_changed") setError(c("stateChanged"));
      else if (code === "account_exists") setError(c("accountExists"));
      else if (code === "invalid_credentials") setError(c("reauthMismatch"));
      else setError(c("unavailableBody"));
    },
    [c, sessionLost],
  );

  // callWithCSRF refreshes the session-bound proof once when a request is
  // refused for a stale token; a lost session is reported to the app.
  const callWithCSRF = useCallback(
    async <T,>(action: (token: string) => Promise<T>): Promise<T> => {
      try {
        return await action(csrf);
      } catch (err) {
        if (err instanceof AuthAPIError && err.code === "csrf") {
          try {
            const refreshed = await authApi.profile();
            setCSRF(refreshed.csrfToken);
            setProfile(refreshed.profile);
            return await action(refreshed.csrfToken);
          } catch (refreshErr) {
            handleError(refreshErr);
            throw refreshErr;
          }
        }
        throw err;
      }
    },
    [csrf, handleError],
  );

  const signOut = useCallback(async () => {
    setError("");
    try {
      await authApi.signOutCurrent(csrf);
    } catch {
      // The server session may still be valid: stay signed in and say so.
      setError(c("signOutFailed"));
      return;
    }
    sessionLost();
  }, [c, csrf, sessionLost]);

  function openDialog(action: PendingAction) {
    invoker.current = document.activeElement as HTMLElement | null;
    setPending(action);
  }

  function closeDialog() {
    setPending(null);
    invoker.current?.focus();
  }

  async function runQuietly(action: () => Promise<void>) {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      await action();
    } catch (err) {
      handleError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="app-shell">
      <header className="account-header">
        <div>
          <span className="eyebrow">{c("eyebrow")}</span>
          <strong>
            {c("signedInAs")} {profile.displayName || profile.email}
          </strong>
        </div>
        <nav aria-label={c("eyebrow")} className="account-nav">
          {onOpenWork && (
            <button type="button" onClick={onOpenWork}>
              {c("navWork")}
            </button>
          )}
          {(["profile", "sessions"] as View[]).map((item) => (
            <button
              key={item}
              type="button"
              aria-current={view === item ? "page" : undefined}
              className={view === item ? "active" : ""}
              onClick={() => setView(item)}
            >
              {item === "profile" ? c("navProfile") : c("navSessions")}
            </button>
          ))}
          {profile.role === "admin" && (
            <button
              type="button"
              aria-current={view === "requests" ? "page" : undefined}
              className={view === "requests" ? "active" : ""}
              onClick={() => setView("requests")}
            >
              {c("navRequests")}
            </button>
          )}
          <button type="button" onClick={() => void signOut()}>
            {c("signOut")}
          </button>
        </nav>
      </header>
      <main className="account-main">
        {error && (
          <div className="form-error" role="alert">
            {error}
          </div>
        )}
        {notice && (
          <div className="auth-notice" role="status">
            {notice}
          </div>
        )}
        {view === "profile" && (
          <ProfilePanel
            profile={profile}
            copy={c}
            busy={busy}
            onSaved={(updated) => {
              setProfile(updated);
              setLocale(updated.locale);
            }}
            onReload={async () => {
              try {
                const refreshed = await authApi.profile();
                setProfile(refreshed.profile);
                setCSRF(refreshed.csrfToken);
                setLocale(refreshed.profile.locale);
              } catch (err) {
                handleError(err);
              }
            }}
            setNotice={setNotice}
            withCSRF={callWithCSRF}
            onError={handleError}
          />
        )}
        {view === "sessions" && (
          <SessionsPanel
            copy={c}
            locale={locale}
            busy={busy}
            setNotice={setNotice}
            withCSRF={callWithCSRF}
            onError={handleError}
            confirm={(dialog) => setConfirm(dialog)}
            onSignedOut={() => sessionLost()}
          />
        )}
        {view === "requests" && profile.role === "admin" && (
          <OrganizationLanguagePanel
            copy={c}
            setNotice={setNotice}
            withCSRF={callWithCSRF}
            onError={handleError}
            onSaved={async () => {
              // People who follow the organisation, including this Admin,
              // switch language with it; others keep their own.
              try {
                const refreshed = await authApi.profile();
                setProfile(refreshed.profile);
                setCSRF(refreshed.csrfToken);
                setLocale(refreshed.profile.locale);
                return refreshed.profile.locale;
              } catch (err) {
                handleError(err);
                return undefined;
              }
            }}
          />
        )}
        {view === "requests" && profile.role === "admin" && (
          <AccessRequestsPanel
            copy={c}
            locale={locale}
            busy={busy}
            setNotice={setNotice}
            withCSRF={callWithCSRF}
            onError={handleError}
            requireReauth={(run) => openDialog({ run })}
          />
        )}
      </main>

      {confirm && (
        <Dialog title={confirm.title} onClose={() => setConfirm(null)} copy={c}>
          <p>{confirm.body}</p>
          <div className="dialog-actions">
            <button type="button" onClick={() => setConfirm(null)}>
              {c("revokeCancel")}
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={() => {
                const action = confirm;
                setConfirm(null);
                void runQuietly(action.run);
              }}
            >
              {confirm.confirmLabel}
            </button>
          </div>
        </Dialog>
      )}

      {pending && (
        <ReauthDialog
          copy={c}
          busy={busy}
          onCancel={closeDialog}
          onSubmit={(password) => {
            const action = pending;
            void runQuietly(async () => {
              await callWithCSRF((token) =>
                authApi.reauthenticate(token, password),
              );
              setPending(null);
              invoker.current?.focus();
              await action.run();
            });
          }}
        />
      )}
    </div>
  );
}

interface ProfilePanelProps {
  profile: AuthProfile;
  copy: SettingsT;
  busy: boolean;
  onSaved: (profile: AuthProfile) => void;
  onReload: () => Promise<void>;
  setNotice: (notice: string) => void;
  withCSRF: <T>(action: (csrf: string) => Promise<T>) => Promise<T>;
  onError: (err: unknown) => void;
}

function ProfilePanel({
  profile,
  copy,
  busy,
  onSaved,
  onReload,
  setNotice,
  withCSRF,
  onError,
}: ProfilePanelProps) {
  const [displayName, setDisplayName] = useState(profile.displayName);
  // The language is a pending value: the UI keeps the saved language until the
  // server confirms the update (COV-PROFILE-001). "" follows the organisation
  // default (DES-HOR-670-02).
  const [pendingLocale, setPendingLocale] = useState<LocaleCode | "">(
    profile.localePreference,
  );
  const [saving, setSaving] = useState(false);
  const [fieldError, setFieldError] = useState("");
  const [conflict, setConflict] = useState(false);

  useEffect(() => {
    setDisplayName(profile.displayName);
    setPendingLocale(profile.localePreference);
  }, [profile]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (displayName.trim() === "") {
      setFieldError(copy("displayName"));
      return;
    }
    setFieldError("");
    setConflict(false);
    setSaving(true);
    setNotice("");
    try {
      const result = await withCSRF((token) =>
        authApi.updateProfile(token, {
          displayName: displayName.trim(),
          locale: pendingLocale,
          expectedUpdatedAt: profile.updatedAt,
        }),
      );
      onSaved(result.profile);
      // The saved language becomes the active UI language, so the confirmation
      // is rendered in that language too.
      setNotice(fixedT(result.profile.locale, "settings")("profileSaved"));
    } catch (err) {
      if (err instanceof AuthAPIError && err.code === "profile_conflict") {
        setConflict(true);
      } else {
        onError(err);
      }
    } finally {
      setSaving(false);
    }
  }

  return (
    <section className="account-card" aria-busy={busy || saving}>
      <h2>{copy("profileTitle")}</h2>
      <p className="auth-intro">{copy("profileIntro")}</p>
      {fieldError && (
        <div className="form-error" role="alert">
          {fieldError}
        </div>
      )}
      {conflict && (
        <div className="form-error" role="alert">
          {copy("profileConflict")}{" "}
          <button
            type="button"
            onClick={() => {
              setConflict(false);
              void onReload();
            }}
          >
            {copy("retry")}
          </button>
        </div>
      )}
      <form className="auth-form" onSubmit={submit}>
        <label>
          <span>{copy("displayName")}</span>
          <input
            value={displayName}
            maxLength={200}
            onChange={(event) => setDisplayName(event.target.value)}
          />
        </label>
        <label>
          <span>{copy("language")}</span>
          <select
            value={pendingLocale}
            onChange={(event) =>
              setPendingLocale(event.target.value as LocaleCode | "")
            }
          >
            <option value="">
              {profile.organizationLocale
                ? copy("languageFollowOrganization", {
                    language: copy(
                      `languageName.${profile.organizationLocale}`,
                    ),
                  })
                : copy("languageFollowBrowser")}
            </option>
            <option value="en">{copy("languageName.en")}</option>
            <option value="pt">{copy("languageName.pt")}</option>
          </select>
        </label>
        <label>
          <span>{copy("profileEmailReadonly")}</span>
          <input value={profile.email} readOnly disabled />
        </label>
        <label>
          <span>{copy("profileRoleReadonly")}</span>
          <input
            value={
              profile.role === "admin"
                ? copy("roleAdmin")
                : copy("roleOperator")
            }
            readOnly
            disabled
          />
        </label>
        <button type="submit" disabled={saving}>
          {saving ? copy("profileSaving") : copy("profileSave")}
        </button>
      </form>
    </section>
  );
}

interface OrganizationLanguagePanelProps {
  copy: SettingsT;
  setNotice: (notice: string) => void;
  withCSRF: <T>(action: (csrf: string) => Promise<T>) => Promise<T>;
  onError: (err: unknown) => void;
  /** Refreshes the viewer's profile and returns their effective locale. */
  onSaved: () => Promise<LocaleCode | undefined>;
}

// The organisation default language (DES-HOR-670-02): Admin-only and logged
// with the Admin's name. HOR-585 moves it to Settings → People.
function OrganizationLanguagePanel({
  copy,
  setNotice,
  withCSRF,
  onError,
  onSaved,
}: OrganizationLanguagePanelProps) {
  const [settings, setSettings] = useState<OrganizationSettings | null>(null);
  const [pending, setPending] = useState<LocaleCode | "">("");
  const [loadFailed, setLoadFailed] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [saving, setSaving] = useState(false);
  const onErrorRef = useRef(onError);
  onErrorRef.current = onError;

  // Loads on mount and on explicit retry only: a shell re-render must never
  // reset the Admin's unsaved choice.
  const load = useCallback(async () => {
    setLoadFailed(false);
    setConflict(false);
    try {
      const loaded = await authApi.organizationSettings();
      setSettings(loaded);
      setPending(loaded.defaultLocale);
    } catch (err) {
      if (err instanceof AuthAPIError && err.status === 403) {
        onErrorRef.current(err);
        return;
      }
      setLoadFailed(true);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!settings || pending === "") return;
    const chosen = pending;
    setSaving(true);
    setConflict(false);
    setNotice("");
    try {
      const updated = await withCSRF((token) =>
        authApi.updateOrganizationSettings(token, {
          defaultLocale: chosen,
          expectedUpdatedAt: settings.updatedAt,
        }),
      );
      setSettings(updated);
      // The confirmation uses the viewer's resolved language, which is the
      // new default only if they follow the organisation.
      const viewerLocale = await onSaved();
      setNotice(
        (viewerLocale ? fixedT(viewerLocale, "settings") : copy)(
          "organizationLanguageSaved",
        ),
      );
    } catch (err) {
      if (err instanceof AuthAPIError && err.code === "settings_conflict") {
        setConflict(true);
      } else {
        onError(err);
      }
    } finally {
      setSaving(false);
    }
  }

  return (
    <section className="account-card" aria-busy={settings === null || saving}>
      <h2>{copy("organizationLanguageTitle")}</h2>
      <p className="auth-intro">{copy("organizationLanguageIntro")}</p>
      {settings === null && !loadFailed && (
        <p className="loading" role="status">
          {copy("loading")}
        </p>
      )}
      {loadFailed && (
        <div className="form-error" role="alert">
          {copy("unavailableBody")}{" "}
          <button type="button" onClick={() => void load()}>
            {copy("retry")}
          </button>
        </div>
      )}
      {conflict && (
        <div className="form-error" role="alert">
          {copy("settingsConflict")}{" "}
          <button type="button" onClick={() => void load()}>
            {copy("retry")}
          </button>
        </div>
      )}
      {settings && (
        <form className="auth-form" onSubmit={submit}>
          <label>
            <span>{copy("organizationLanguageLabel")}</span>
            <select
              value={pending}
              onChange={(event) =>
                setPending(event.target.value as LocaleCode | "")
              }
            >
              {settings.defaultLocale === "" && (
                <option value="" disabled>
                  {copy("organizationLanguageNotSet")}
                </option>
              )}
              <option value="en">{copy("languageName.en")}</option>
              <option value="pt">{copy("languageName.pt")}</option>
            </select>
          </label>
          <button
            type="submit"
            disabled={
              saving || pending === "" || pending === settings.defaultLocale
            }
          >
            {saving ? copy("profileSaving") : copy("organizationLanguageSave")}
          </button>
        </form>
      )}
    </section>
  );
}

interface SessionsPanelProps {
  copy: SettingsT;
  locale: LocaleCode;
  busy: boolean;
  setNotice: (notice: string) => void;
  withCSRF: <T>(action: (csrf: string) => Promise<T>) => Promise<T>;
  onError: (err: unknown) => void;
  confirm: (confirm: Confirm) => void;
  onSignedOut: () => void;
}

function SessionsPanel({
  copy,
  locale,
  busy,
  setNotice,
  withCSRF,
  onError,
  confirm,
  onSignedOut,
}: SessionsPanelProps) {
  const [sessions, setSessions] = useState<SessionInfo[] | null>(null);
  const [loadError, setLoadError] = useState("");
  const load = useCallback(async () => {
    setLoadError("");
    try {
      const result = await authApi.sessions();
      setSessions(result);
    } catch (err) {
      if (err instanceof AuthAPIError && err.status === 401) {
        onSignedOut();
        return;
      }
      setLoadError(copy("sessionsError"));
    }
  }, [copy("sessionsError"), onSignedOut]);

  useEffect(() => {
    void load();
  }, [load]);

  const others = sessions?.filter((session) => !session.current) ?? [];

  return (
    <section className="account-card" aria-busy={busy || sessions === null}>
      <h2>{copy("sessionsTitle")}</h2>
      <p className="auth-intro">{copy("sessionsIntro")}</p>
      {sessions === null && !loadError && (
        <p className="loading" role="status">
          {copy("sessionsLoading")}
        </p>
      )}
      {loadError && (
        <div className="form-error" role="alert">
          {loadError}{" "}
          <button type="button" onClick={() => void load()}>
            {copy("retry")}
          </button>
        </div>
      )}
      {sessions && (
        <>
          <ul className="session-list">
            {sessions.map((session) => (
              <li key={session.id} className={session.current ? "current" : ""}>
                <div>
                  <strong>{sessionLabel(session, copy)}</strong>
                  {session.current && (
                    <span className="badge">{copy("currentSession")}</span>
                  )}
                </div>
                <dl>
                  <div>
                    <dt>{copy("created")}</dt>
                    <dd>{formatTime(session.createdAt, locale)}</dd>
                  </div>
                  <div>
                    <dt>{copy("lastActive")}</dt>
                    <dd>
                      {session.lastActiveAt
                        ? formatTime(session.lastActiveAt, locale)
                        : copy("activityUnavailable")}
                    </dd>
                  </div>
                </dl>
                <button
                  type="button"
                  onClick={() =>
                    session.current
                      ? confirm({
                          title: copy("revokeSessionConfirm"),
                          body: copy("revokeSessionConsequence"),
                          confirmLabel: copy("signOutThisDevice"),
                          run: async () => {
                            try {
                              await withCSRF((token) => authApi.signOut(token));
                            } finally {
                              onSignedOut();
                            }
                          },
                        })
                      : confirm({
                          title: copy("revokeSessionConfirm"),
                          body: copy("revokeSessionConsequence"),
                          confirmLabel: copy("revokeConfirm"),
                          run: async () => {
                            await withCSRF((token) =>
                              authApi.revokeSession(token, session.id),
                            );
                            setNotice(copy("revoked"));
                            await load();
                          },
                        })
                  }
                >
                  {session.current
                    ? copy("signOutThisDevice")
                    : copy("signOutSession")}
                </button>
              </li>
            ))}
          </ul>
          {others.length > 0 && (
            <button
              type="button"
              onClick={() =>
                confirm({
                  title: copy("revokeOthersConfirm"),
                  body: copy("revokeOthersConsequence"),
                  confirmLabel: copy("revokeOthers"),
                  run: async () => {
                    await withCSRF((token) =>
                      authApi.revokeOtherSessions(token),
                    );
                    setNotice(copy("revoked"));
                    await load();
                  },
                })
              }
            >
              {copy("revokeOthers")} ({formatNumber(others.length, locale)})
            </button>
          )}
        </>
      )}
    </section>
  );
}

function sessionLabel(session: SessionInfo, copy: SettingsT): string {
  const client = [session.browser, session.os, session.device]
    .filter(Boolean)
    .join(" · ");
  const region = session.region || session.country || copy("regionUnavailable");
  if (!client)
    return region === copy("regionUnavailable")
      ? copy("clientUnavailable")
      : region;
  return `${client} — ${region}`;
}

function formatTime(value: string, locale: LocaleCode): string {
  return formatDateTime(value, locale);
}

interface AccessRequestsPanelProps {
  copy: SettingsT;
  locale: LocaleCode;
  busy: boolean;
  setNotice: (notice: string) => void;
  withCSRF: <T>(action: (csrf: string) => Promise<T>) => Promise<T>;
  onError: (err: unknown) => void;
  requireReauth: (run: () => Promise<void>) => void;
}

function AccessRequestsPanel({
  copy,
  locale,
  busy,
  setNotice,
  withCSRF,
  onError,
  requireReauth,
}: AccessRequestsPanelProps) {
  const [requests, setRequests] = useState<AccessRequest[] | null>(null);
  const [loadError, setLoadError] = useState("");
  const [review, setReview] = useState<AccessRequest | null>(null);
  const [role, setRole] = useState<Role | "">("");

  const load = useCallback(async () => {
    setLoadError("");
    try {
      setRequests(await authApi.accessRequests());
    } catch (err) {
      if (err instanceof AuthAPIError && err.status === 403) {
        onError(err);
        setRequests([]);
        return;
      }
      setLoadError(copy("requestsError"));
    }
  }, [copy("requestsError"), onError]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <section className="account-card" aria-busy={busy || requests === null}>
      <h2>{copy("requestsTitle")}</h2>
      <p className="auth-intro">{copy("requestsIntro")}</p>
      {requests === null && !loadError && (
        <p className="loading" role="status">
          {copy("requestsLoading")}
        </p>
      )}
      {loadError && (
        <div className="form-error" role="alert">
          {loadError}{" "}
          <button type="button" onClick={() => void load()}>
            {copy("retry")}
          </button>
        </div>
      )}
      {requests && requests.length === 0 && <p>{copy("requestsEmpty")}</p>}
      {requests && requests.length > 0 && (
        <ul className="request-list">
          {requests.map((request) => (
            <li key={request.id}>
              <div>
                <strong>{request.email}</strong>
                {request.verifiedAt && (
                  <span className="badge">
                    {copy("verifyVerified")}{" "}
                    {formatTime(request.verifiedAt, locale)}
                  </span>
                )}
              </div>
              <div className="dialog-actions">
                <button type="button" onClick={() => setReview(request)}>
                  {copy("requestsReview")}
                </button>
                <button
                  type="button"
                  onClick={() =>
                    requireReauth(async () => {
                      try {
                        await withCSRF((token) =>
                          authApi.decline(token, request.id),
                        );
                        setNotice(copy("declineDone"));
                        await load();
                      } catch (err) {
                        onError(err);
                      }
                    })
                  }
                >
                  {copy("requestsDecline")}
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}

      {review && (
        <Dialog
          title={copy("approveTitle")}
          onClose={() => setReview(null)}
          copy={copy}
        >
          <p>
            {copy("approveIntro")} {review.email}
          </p>
          <fieldset>
            <legend>{copy("approveAs")}</legend>
            <label>
              <input
                type="radio"
                name="role"
                value="operator"
                checked={role === "operator"}
                onChange={() => setRole("operator")}
              />
              <span>{copy("roleOperator")}</span>
            </label>
            <label>
              <input
                type="radio"
                name="role"
                value="admin"
                checked={role === "admin"}
                onChange={() => setRole("admin")}
              />
              <span>{copy("roleAdmin")}</span>
            </label>
          </fieldset>
          <div className="dialog-actions">
            <button type="button" onClick={() => setReview(null)}>
              {copy("reauthCancel")}
            </button>
            <button
              type="button"
              disabled={role === ""}
              onClick={() => {
                const target = review;
                const chosen = role;
                if (!chosen) return;
                setReview(null);
                requireReauth(async () => {
                  try {
                    await withCSRF((token) =>
                      authApi.approve(token, target.id, chosen),
                    );
                    setNotice(copy("approveDone"));
                    await load();
                  } catch (err) {
                    onError(err);
                  }
                });
              }}
            >
              {copy("approveSubmit")}
            </button>
          </div>
        </Dialog>
      )}
    </section>
  );
}

function Dialog({
  title,
  children,
  onClose,
  copy,
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
  copy: SettingsT;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const node = ref.current;
    const focusable = node?.querySelector<HTMLElement>(
      "input, button, select, textarea, [tabindex]",
    );
    focusable?.focus();
    return () => previous?.focus();
  }, []);

  return (
    <div className="overlay">
      <div
        className="account-card dialog"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        ref={ref}
        onKeyDown={(event) => {
          if (event.key === "Escape") onClose();
        }}
      >
        <h2>{title}</h2>
        {children}
        {copy("retry") === "" && null}
      </div>
    </div>
  );
}

function ReauthDialog({
  copy,
  busy,
  onCancel,
  onSubmit,
}: {
  copy: SettingsT;
  busy: boolean;
  onCancel: () => void;
  onSubmit: (password: string) => void;
}) {
  const [password, setPassword] = useState("");
  return (
    <Dialog title={copy("reauthTitle")} onClose={onCancel} copy={copy}>
      <p>{copy("reauthIntro")}</p>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          onSubmit(password);
        }}
      >
        <label>
          <span>{copy("password")}</span>
          <input
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
        </label>
        <div className="dialog-actions">
          <button type="button" onClick={onCancel}>
            {copy("reauthCancel")}
          </button>
          <button type="submit" disabled={busy || password === ""}>
            {copy("reauthSubmit")}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
