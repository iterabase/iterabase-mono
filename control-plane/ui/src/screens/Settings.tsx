import { Link } from "@tanstack/react-router";
import { useCallback, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useSession } from "../app/session";
import { TrustIcon } from "../components/brand";
import { Denied, NotFound } from "../components/states";
import { AuthAPIError, authApi } from "../auth/authApi";
import { fixedT } from "../i18n";
import {
  AccessRequestsPanel,
  Dialog,
  OrganizationLanguagePanel,
  ProfilePanel,
  ReauthDialog,
  SessionsPanel,
  type Confirm,
} from "../settings/panels";

// Settings (from the avatar menu, or More on phone) with in-page tabs. Profile
// and Sessions now; People hosts the Admin access requests and organisation
// language until HOR-585. API keys (HOR-514), Integrations (HOR-652), Runners
// (HOR-596), Plan & usage (HOR-658) and Sign-in & security (HOR-314) add their
// tabs here.

export type SettingsTab = "profile" | "sessions" | "people";
export const settingsTabs: SettingsTab[] = ["profile", "sessions", "people"];
const adminOnly = new Set<SettingsTab>(["people"]);

export function isSettingsTab(value: string): value is SettingsTab {
  return (settingsTabs as string[]).includes(value);
}

export function Settings({ tab }: { tab: string }) {
  const { t } = useTranslation("app");
  const session = useSession();
  const c = useMemo(() => fixedT(session.locale, "settings"), [session.locale]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [pending, setPending] = useState<(() => Promise<void>) | null>(null);
  const invoker = useRef<HTMLElement | null>(null);

  const handleError = useCallback(
    (err: unknown) => {
      if (session.lost(err)) return;
      const code = err instanceof AuthAPIError ? err.code : "unavailable";
      if (code === "recent_auth_required") return setError("");
      if (
        code === "forbidden" ||
        (err instanceof AuthAPIError && err.status === 403)
      ) {
        // The role may have changed: re-read it so Admin-only UI unmounts.
        void session.recheck();
        return setError(c("forbidden"));
      }
      if (code === "throttled") return setError(c("throttled"));
      if (code === "state_changed") return setError(c("stateChanged"));
      if (code === "account_exists") return setError(c("accountExists"));
      if (code === "invalid_credentials") return setError(c("reauthMismatch"));
      setError(c("unavailableBody"));
    },
    [c, session],
  );

  const run = async (action: () => Promise<void>) => {
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
  };

  if (!isSettingsTab(tab)) return <NotFound />;
  const visible = settingsTabs.filter(
    (entry) => session.isAdmin || !adminOnly.has(entry),
  );
  const label = (entry: SettingsTab) =>
    entry === "profile"
      ? c("navProfile")
      : entry === "sessions"
        ? c("navSessions")
        : t("people");

  return (
    <>
      <header className="page-head">
        <div>
          <p className="ib-eyebrow m-0">
            {session.isAdmin ? t("roleAdmin") : t("roleOperator")}
          </p>
          <h1 className="ib-hero ib-hero--page mt-2">{t("settings")}</h1>
        </div>
        <p className="ib-trust m-0">
          <TrustIcon />
          {t("settingsLogged")}
        </p>
      </header>
      <nav aria-label={t("settings")} className="settings-tabs">
        {visible.map((entry) => (
          <Link
            key={entry}
            to="/settings/$tab"
            params={{ tab: entry }}
            aria-current={entry === tab ? "page" : undefined}
          >
            {label(entry)}
          </Link>
        ))}
      </nav>
      {adminOnly.has(tab) && !session.isAdmin ? (
        <Denied />
      ) : (
        <div className="max-w-[720px]">
          {error && (
            <div className="form-error mb-4" role="alert">
              {error}
            </div>
          )}
          {notice && (
            <div className="auth-notice" role="status">
              {notice}
            </div>
          )}
          {tab === "profile" && (
            <ProfilePanel
              profile={session.profile}
              copy={c}
              busy={busy}
              onSaved={(updated) => session.updateProfile(updated)}
              onReload={() => session.recheck()}
              setNotice={setNotice}
              withCSRF={session.withCSRF}
              onError={handleError}
            />
          )}
          {tab === "sessions" && (
            <SessionsPanel
              copy={c}
              locale={session.locale}
              busy={busy}
              setNotice={setNotice}
              withCSRF={session.withCSRF}
              onError={handleError}
              confirm={setConfirm}
              onSignedOut={() => session.expire()}
            />
          )}
          {tab === "people" && (
            <>
              <OrganizationLanguagePanel
                copy={c}
                setNotice={setNotice}
                withCSRF={session.withCSRF}
                onError={handleError}
                onSaved={async () => {
                  // People who follow the organisation, this Admin included,
                  // switch language with it.
                  try {
                    const refreshed = await authApi.profile();
                    session.updateProfile(refreshed.profile);
                    return refreshed.profile.locale;
                  } catch (err) {
                    handleError(err);
                    return undefined;
                  }
                }}
              />
              <AccessRequestsPanel
                copy={c}
                locale={session.locale}
                busy={busy}
                setNotice={setNotice}
                withCSRF={session.withCSRF}
                onError={handleError}
                requireReauth={(action) => {
                  invoker.current =
                    document.activeElement as HTMLElement | null;
                  setPending(() => action);
                }}
              />
            </>
          )}
        </div>
      )}

      {confirm && (
        <Dialog title={confirm.title} onClose={() => setConfirm(null)}>
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
                void run(action.run);
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
          onCancel={() => {
            setPending(null);
            invoker.current?.focus();
          }}
          onSubmit={(password) => {
            const action = pending;
            void run(async () => {
              await session.withCSRF((csrf) =>
                authApi.reauthenticate(csrf, password),
              );
              setPending(null);
              await action();
            });
          }}
        />
      )}
    </>
  );
}
