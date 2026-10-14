import {
  FormEvent,
  ReactNode,
  useCallback,
  useEffect,
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
} from "../auth/authApi";
import type { TFunction } from "i18next";
import { fixedT } from "../i18n";
import {
  Dialog as RadixDialog,
  DialogContent,
  DialogTitle,
} from "../components/ui/dialog";
import { formatDateTime, formatNumber } from "../i18n/format";
import type { AccessRequests } from "./useAccessRequests";

// The panels hosted by the Settings shell (HOR-459), in the XTSettingsPeople
// board's language: cards with a title row, quiet column lists, ink and ghost
// buttons, the role select. Profile and Sessions for everyone; the access
// requests and the organisation language for Admins under People
// (DES-HOR-670-02).

export type SettingsT = TFunction<"settings">;

export interface Confirm {
  title: string;
  body: string;
  confirmLabel: string;
  run: () => Promise<void>;
}

/** A card with the board's title row: title · muted count · action. */
function Panel({
  title,
  count,
  action,
  busy,
  children,
}: {
  title: string;
  count?: string;
  action?: ReactNode;
  busy?: boolean;
  children: ReactNode;
}) {
  return (
    <section className="card overflow-hidden" aria-busy={busy}>
      <div className="panel-head">
        <div className="panel-title">
          <h2>{title}</h2>
          {count && <span className="muted text-[13.5px]">{count}</span>}
        </div>
        {action && <span className="ml-auto">{action}</span>}
      </div>
      {children}
    </section>
  );
}

function Failure({
  message,
  copy,
  onRetry,
}: {
  message: string;
  copy: SettingsT;
  onRetry: () => void;
}) {
  return (
    <div className="form-error" role="alert">
      {message}{" "}
      <button type="button" className="link-button" onClick={onRetry}>
        {copy("retry")}
      </button>
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

export function ProfilePanel({
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
    <Panel title={copy("profileTitle")} busy={busy || saving}>
      <div className="panel-body">
        {fieldError && (
          <div className="form-error" role="alert">
            {fieldError}
          </div>
        )}
        {conflict && (
          <Failure
            message={copy("profileConflict")}
            copy={copy}
            onRetry={() => {
              setConflict(false);
              void onReload();
            }}
          />
        )}
        <form
          className="grid gap-[14px] desk:grid-cols-2 desk:gap-x-[18px]"
          onSubmit={submit}
        >
          <label className="field">
            <span>{copy("displayName")}</span>
            <input
              value={displayName}
              maxLength={200}
              autoComplete="name"
              onChange={(event) => setDisplayName(event.target.value)}
            />
          </label>
          <label className="field">
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
          <label className="field">
            <span>{copy("profileEmailReadonly")}</span>
            <input value={profile.email} readOnly disabled />
          </label>
          <label className="field">
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
          <div className="flex justify-end pt-[4px] desk:col-span-2">
            <button
              type="submit"
              className="btn btn-ink max-desk:w-full"
              disabled={saving}
            >
              {saving ? copy("profileSaving") : copy("profileSave")}
            </button>
          </div>
        </form>
      </div>
    </Panel>
  );
}

interface OrganizationLanguagePanelProps {
  copy: SettingsT;
  setNotice: (notice: string) => void;
  withCSRF: <T>(action: (csrf: string) => Promise<T>) => Promise<T>;
  onError: (err: unknown) => void;
  /** Refreshes the viewer's profile and returns their effective locale. */
  onSaved: () => Promise<LocaleCode | undefined>;
  /** The installation's name changed (DES-HOR-459-08). */
  onNameSaved: (name: string) => void;
}

/** The longest installation name the API accepts (DES-HOR-459-08). */
const maxOrganizationName = 80;

// The organisation default language (DES-HOR-670-02): Admin-only and logged
// with the Admin's name.
export function OrganizationLanguagePanel({
  copy,
  setNotice,
  withCSRF,
  onError,
  onSaved,
  onNameSaved,
}: OrganizationLanguagePanelProps) {
  const [settings, setSettings] = useState<OrganizationSettings | null>(null);
  const [pending, setPending] = useState<LocaleCode | "">("");
  const [pendingName, setPendingName] = useState("");
  const [savingName, setSavingName] = useState(false);
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
      setPendingName(loaded.name ?? "");
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

  async function submitName(event: FormEvent) {
    event.preventDefault();
    if (!settings) return;
    const name = pendingName.trim();
    setSavingName(true);
    setConflict(false);
    setNotice("");
    try {
      const updated = await withCSRF((token) =>
        authApi.updateOrganizationSettings(token, {
          name,
          expectedUpdatedAt: settings.updatedAt,
        }),
      );
      setSettings(updated);
      setPendingName(updated.name ?? "");
      onNameSaved(updated.name ?? "");
      setNotice(copy("organizationNameSaved"));
    } catch (err) {
      if (err instanceof AuthAPIError && err.code === "settings_conflict") {
        setConflict(true);
      } else {
        onError(err);
      }
    } finally {
      setSavingName(false);
    }
  }

  return (
    <Panel
      title={copy("organizationTitle")}
      busy={settings === null || saving || savingName}
    >
      <div className="panel-body">
        {settings === null && !loadFailed && (
          <p className="muted m-0 text-[13.5px]" role="status">
            {copy("loading")}
          </p>
        )}
        {loadFailed && (
          <Failure
            message={copy("unavailableBody")}
            copy={copy}
            onRetry={() => void load()}
          />
        )}
        {conflict && (
          <Failure
            message={copy("settingsConflict")}
            copy={copy}
            onRetry={() => void load()}
          />
        )}
        {settings && (
          <form
            className="flex flex-wrap items-end gap-[10px]"
            onSubmit={submitName}
          >
            <label className="field min-w-[200px] flex-1">
              <span>{copy("organizationNameLabel")}</span>
              <input
                value={pendingName}
                maxLength={maxOrganizationName}
                autoComplete="organization"
                onChange={(event) => setPendingName(event.target.value)}
              />
            </label>
            <button
              type="submit"
              className="btn btn-secondary settings-field-btn"
              disabled={
                savingName || pendingName.trim() === (settings.name ?? "")
              }
            >
              {savingName
                ? copy("profileSaving")
                : copy("organizationNameSave")}
            </button>
            <p className="muted m-0 basis-full text-[13px]">
              {copy("organizationNameHelp")}
            </p>
          </form>
        )}
        <p className="auth-intro">{copy("organizationLanguageIntro")}</p>
        {settings && (
          <form
            className="flex flex-wrap items-end gap-[10px]"
            onSubmit={submit}
          >
            <label className="field min-w-[200px] flex-1">
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
              className="btn btn-secondary settings-field-btn"
              disabled={
                saving || pending === "" || pending === settings.defaultLocale
              }
            >
              {saving
                ? copy("profileSaving")
                : copy("organizationLanguageSave")}
            </button>
          </form>
        )}
      </div>
    </Panel>
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

export function SessionsPanel({
  copy,
  locale,
  busy,
  setNotice,
  withCSRF,
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
    <Panel
      title={copy("sessionsTitle")}
      count={
        sessions && sessions.length > 0
          ? copy("sessionsCount", {
              count: sessions.length,
            })
          : undefined
      }
      busy={busy || sessions === null}
      action={
        others.length > 0 && (
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            onClick={() =>
              confirm({
                title: copy("revokeOthersConfirm"),
                body: copy("revokeOthersConsequence"),
                confirmLabel: copy("revokeOthers"),
                run: async () => {
                  await withCSRF((token) => authApi.revokeOtherSessions(token));
                  setNotice(copy("revoked"));
                  await load();
                },
              })
            }
          >
            {copy("revokeOthers")} ({formatNumber(others.length, locale)})
          </button>
        )
      }
    >
      {sessions === null && !loadError && (
        <p className="panel-body muted m-0 text-[13.5px]" role="status">
          {copy("sessionsLoading")}
        </p>
      )}
      {loadError && (
        <div className="panel-body">
          <Failure
            message={loadError}
            copy={copy}
            onRetry={() => void load()}
          />
        </div>
      )}
      {sessions && (
        <>
          <div className="session-cols" aria-hidden="true">
            <span className="eyebrow">{copy("sessionColumn")}</span>
            <span className="eyebrow">{copy("created")}</span>
            <span className="eyebrow">{copy("lastActive")}</span>
            <span />
          </div>
          <ul className="list session-list">
            {sessions.map((session) => (
              <li key={session.id} className="session-row">
                <span className="flex min-w-0 flex-wrap items-center gap-x-[10px] gap-y-[6px]">
                  <b className="text-[14.5px] font-semibold">
                    {sessionLabel(session, copy)}
                  </b>
                  {session.current && (
                    <span className="badge session-current">
                      {copy("currentSession")}
                    </span>
                  )}
                </span>
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
                <span className="session-action">
                  <button
                    type="button"
                    className="btn btn-ghost btn-sm"
                    onClick={() =>
                      session.current
                        ? confirm({
                            title: copy("revokeSessionConfirm"),
                            body: copy("revokeSessionConsequence"),
                            confirmLabel: copy("signOutThisDevice"),
                            run: async () => {
                              try {
                                await withCSRF((token) =>
                                  authApi.signOut(token),
                                );
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
                </span>
              </li>
            ))}
          </ul>
        </>
      )}
    </Panel>
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

/** Two letters from the email's local part: joao.pires@… → JP. */
function emailInitials(email: string): string {
  const parts = (email.split("@")[0] ?? "").split(/[._+-]+/).filter(Boolean);
  const letters =
    parts.length > 1
      ? (parts[0][0] ?? "") + (parts[1][0] ?? "")
      : (parts[0] ?? "").slice(0, 2);
  return letters.toUpperCase() || "·";
}

interface AccessRequestsPanelProps {
  copy: SettingsT;
  locale: LocaleCode;
  busy: boolean;
  access: AccessRequests;
  setNotice: (notice: string) => void;
  withCSRF: <T>(action: (csrf: string) => Promise<T>) => Promise<T>;
  onError: (err: unknown) => void;
  /** Runs the action once the Admin re-enters their password. */
  requireReauth: (run: () => Promise<void>, context: string) => void;
}

// "Waiting for your approval": one card per verified request with the role,
// Decline and Approve. Both decisions need a recent password, and the
// password dialog repeats what is being decided.
export function AccessRequestsPanel({
  copy,
  locale,
  busy,
  access,
  setNotice,
  withCSRF,
  onError,
  requireReauth,
}: AccessRequestsPanelProps) {
  const { requests, loadFailed, reload } = access;
  const [roles, setRoles] = useState<Record<string, Role>>({});

  const roleName = (role: Role) =>
    role === "admin" ? copy("roleAdmin") : copy("roleOperator");

  return (
    <section
      className="flex flex-col gap-[10px]"
      aria-busy={busy || requests === null}
      aria-labelledby="requests-title"
    >
      <div className="panel-title">
        <h2 id="requests-title" className="section-title">
          {copy("requestsWaitingTitle")}
        </h2>
        {requests && requests.length > 0 && (
          <span className="muted text-[13.5px]">
            {copy("requestsCount", {
              count: requests.length,
            })}
          </span>
        )}
      </div>
      {requests === null && !loadFailed && (
        <p className="muted m-0 text-[13.5px]" role="status">
          {copy("requestsLoading")}
        </p>
      )}
      {loadFailed && (
        <Failure
          message={copy("requestsError")}
          copy={copy}
          onRetry={() => void reload()}
        />
      )}
      {requests && requests.length === 0 && (
        <p className="card request-empty">{copy("requestsEmpty")}</p>
      )}
      {requests?.map((request) => {
        const role = roles[request.id] ?? "operator";
        return (
          <RequestCard
            key={request.id}
            request={request}
            copy={copy}
            locale={locale}
            role={role}
            onRole={(value) =>
              setRoles((current) => ({ ...current, [request.id]: value }))
            }
            onDecline={() =>
              requireReauth(
                async () => {
                  try {
                    await withCSRF((token) =>
                      authApi.decline(token, request.id),
                    );
                    setNotice(copy("declineDone"));
                    await reload();
                  } catch (err) {
                    onError(err);
                  }
                },
                `${copy("declineContext", { email: request.email })} ${copy("declineIntro")}`,
              )
            }
            onApprove={() =>
              requireReauth(
                async () => {
                  try {
                    await withCSRF((token) =>
                      authApi.approve(token, request.id, role),
                    );
                    setNotice(copy("approveDone"));
                    await reload();
                  } catch (err) {
                    onError(err);
                  }
                },
                `${copy("approveContext", { email: request.email, role: roleName(role) })} ${copy("approveIntro")}`,
              )
            }
          />
        );
      })}
    </section>
  );
}

function RequestCard({
  request,
  copy,
  locale,
  role,
  onRole,
  onDecline,
  onApprove,
}: {
  request: AccessRequest;
  copy: SettingsT;
  locale: LocaleCode;
  role: Role;
  onRole: (role: Role) => void;
  onDecline: () => void;
  onApprove: () => void;
}) {
  const who = `request-${request.id}`;
  return (
    <article className="card request-card" aria-labelledby={who}>
      <span className="row gap-[12px]">
        <span className="avatar-chip avatar-chip--lg" aria-hidden="true">
          {emailInitials(request.email)}
        </span>
        <span className="flex min-w-0 flex-col gap-[2px]">
          <b id={who} className="text-[15px] break-words">
            {request.email}
          </b>
          <span className="muted text-[12.5px] desk:text-[13px]">
            {request.verifiedAt
              ? copy("requestVerifiedMeta", {
                  when: formatTime(request.verifiedAt, locale),
                })
              : copy("requestCreatedMeta", {
                  when: formatTime(request.createdAt, locale),
                })}
          </span>
        </span>
      </span>
      <div className="req-actions">
        <label className="req-role">
          <span className="muted text-[13px] max-desk:sr-only">
            {copy("profileRoleReadonly")}
          </span>
          <select
            className="select"
            value={role}
            aria-describedby={who}
            onChange={(event) => onRole(event.target.value as Role)}
          >
            <option value="operator">{copy("roleOperator")}</option>
            <option value="admin">{copy("roleAdmin")}</option>
          </select>
        </label>
        <button
          type="button"
          className="btn btn-secondary req-decline"
          aria-describedby={who}
          onClick={onDecline}
        >
          {copy("requestsDecline")}
        </button>
        <button
          type="button"
          className="btn btn-ink req-approve"
          aria-describedby={who}
          onClick={onApprove}
        >
          {copy("approveSubmit")}
        </button>
      </div>
    </article>
  );
}

export function Dialog({
  title,
  children,
  onClose,
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
}) {
  return (
    <RadixDialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="xt" aria-describedby={undefined}>
        <DialogTitle className="dialog-title">{title}</DialogTitle>
        {children}
      </DialogContent>
    </RadixDialog>
  );
}

export function ReauthDialog({
  copy,
  busy,
  context,
  onCancel,
  onSubmit,
}: {
  copy: SettingsT;
  busy: boolean;
  /** What the password confirms, e.g. which request is approved as what. */
  context?: string;
  onCancel: () => void;
  onSubmit: (password: string) => void;
}) {
  const [password, setPassword] = useState("");
  return (
    <Dialog title={copy("reauthTitle")} onClose={onCancel}>
      {context && <p className="dialog-body dialog-context">{context}</p>}
      <p className="dialog-body">{copy("reauthIntro")}</p>
      <form
        className="mt-[16px]"
        onSubmit={(event) => {
          event.preventDefault();
          onSubmit(password);
        }}
      >
        <label className="field">
          <span>{copy("password")}</span>
          <input
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
        </label>
        <div className="dialog-actions">
          <button type="button" className="btn btn-ghost" onClick={onCancel}>
            {copy("reauthCancel")}
          </button>
          <button
            type="submit"
            className="btn btn-ink"
            disabled={busy || password === ""}
          >
            {copy("reauthSubmit")}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
