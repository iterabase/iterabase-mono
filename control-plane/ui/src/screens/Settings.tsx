import { Link } from "@tanstack/react-router";
import { useCallback, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { PhoneHeader } from "../app/Shell";
import { useSession } from "../app/session";
import { Denied, NotFound } from "../components/states";
import { AuthAPIError, authApi } from "../auth/authApi";
import { BackIcon, LockIcon } from "../auth/icons";
import { fixedT } from "../i18n";
import { formatNumber } from "../i18n/format";
import {
  AccessRequestsPanel,
  Dialog,
  OrganizationLanguagePanel,
  ProfilePanel,
  ReauthDialog,
  SessionsPanel,
  type Confirm,
} from "../settings/panels";
import { useAccessRequests } from "../settings/useAccessRequests";

// Settings (from the avatar menu, or More on phone), ported from
// XTSettingsPeople / XTPhoneSettingsPeople: the role eyebrow, the serif title
// and the logged-with-your-name line, underline tabs, and panels in the
// board's card language. On phone each tab is its own page behind "‹ More".
// API keys (HOR-514), Integrations (HOR-652), Runners (HOR-596), Plan & usage
// (HOR-658) and Sign-in & security (HOR-314) add their tabs here.

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
  const [pending, setPending] = useState<{
    run: () => Promise<void>;
    context: string;
  } | null>(null);
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

  // Waiting requests feed the People panel and the People tab's count.
  const access = useAccessRequests(session.isAdmin, handleError);
  const waiting = access.requests?.length ?? 0;

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
  const intro =
    tab === "profile"
      ? c("profileIntro")
      : tab === "sessions"
        ? c("sessionsIntro")
        : c("peopleIntro");

  return (
    <div className="settings-page">
      <PhoneHeader>
        <Link to="/more" className="phone-back">
          <BackIcon />
          {t("nav.more")}
        </Link>
        <h1 className="phone-title m-0">{label(tab)}</h1>
      </PhoneHeader>

      <header className="settings-head">
        <div className="flex flex-col gap-[10px]">
          <span className="eyebrow">
            {session.isAdmin ? t("roleAdmin") : t("roleOperator")}
          </span>
          <h1 className="serif m-0 text-[34px] leading-[1.08]">
            {t("settings")}
          </h1>
        </div>
        <p className="lock-line">
          <LockIcon />
          <span>{t("settingsLogged")}</span>
        </p>
      </header>

      <nav aria-label={t("settings")} className="settings-tabs">
        {visible.map((entry) => (
          <Link
            key={entry}
            to="/settings/$tab"
            params={{ tab: entry }}
            aria-current={entry === tab ? "page" : undefined}
            aria-describedby={
              entry === "people" && waiting > 0 ? "people-waiting" : undefined
            }
          >
            {label(entry)}
            {entry === "people" && waiting > 0 && (
              <>
                <span className="count-chip" aria-hidden="true">
                  {formatNumber(waiting, session.locale)}
                </span>
                <span id="people-waiting" className="sr-only">
                  {c("toApprove", {
                    count: waiting,
                  })}
                </span>
              </>
            )}
          </Link>
        ))}
      </nav>

      {adminOnly.has(tab) && !session.isAdmin ? (
        <Denied />
      ) : (
        <div className="flex flex-col gap-[12px] desk:gap-[16px]">
          <p className="settings-intro">{intro}</p>
          {error && (
            <div className="form-error max-w-[760px]" role="alert">
              {error}
            </div>
          )}
          {notice && (
            <div className="auth-notice max-w-[760px]" role="status">
              {notice}
            </div>
          )}
          {tab === "profile" && (
            <div className="max-w-[760px]">
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
            </div>
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
            <div className="grid items-start gap-[18px] desk:grid-cols-[minmax(320px,420px)_minmax(0,1fr)]">
              <AccessRequestsPanel
                copy={c}
                locale={session.locale}
                busy={busy}
                access={access}
                setNotice={setNotice}
                withCSRF={session.withCSRF}
                onError={handleError}
                requireReauth={(action, context) => {
                  invoker.current =
                    document.activeElement as HTMLElement | null;
                  setPending({ run: action, context });
                }}
              />
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
            </div>
          )}
        </div>
      )}

      <footer className="settings-foot">
        <p className="lock-line">
          <LockIcon />
          <span>{t("trustLine")}</span>
        </p>
      </footer>

      {confirm && (
        <Dialog title={confirm.title} onClose={() => setConfirm(null)}>
          <p className="dialog-body">{confirm.body}</p>
          <div className="dialog-actions">
            <button
              type="button"
              className="btn btn-ghost"
              onClick={() => setConfirm(null)}
            >
              {c("revokeCancel")}
            </button>
            <button
              type="button"
              className="btn btn-ink"
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
          context={pending.context}
          onCancel={() => {
            setPending(null);
            invoker.current?.focus();
          }}
          onSubmit={(password) => {
            const action = pending.run;
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
    </div>
  );
}
