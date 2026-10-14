import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useSession } from "../app/session";
import { ChevronIcon } from "../auth/icons";
import { fixedT } from "../i18n";
import { useAccessRequests } from "../settings/useAccessRequests";
import { initials } from "../work/model";
import { settingsTabs } from "./Settings";

// Phone "More" (HOR-664 tab), ported from XTPhoneMore: the person, the
// Settings rows with chevrons and Sign out. Reports joins with HOR-658 and the
// other Settings rows with their tickets.

export function More() {
  const { t } = useTranslation("app");
  const session = useSession();
  const c = fixedT(session.locale, "settings");
  const [failed, setFailed] = useState(false);
  const { requests } = useAccessRequests(session.isAdmin);
  const waiting = requests?.length ?? 0;
  const name = session.profile.displayName || session.profile.email;
  const tabs = settingsTabs.filter(
    (tab) => session.isAdmin || tab !== "people",
  );
  return (
    <div className="more-page">
      <h1 className="serif m-0 text-[30px] leading-[1.08]">{t("nav.more")}</h1>
      <div className="card row gap-[12px] p-[14px]">
        <span
          className="inline-flex h-[42px] w-[42px] flex-none items-center justify-center rounded-full bg-[var(--ink)] text-[15px] font-semibold text-[#FBF8F2]"
          aria-hidden="true"
        >
          {initials(name)}
        </span>
        <span className="flex min-w-0 flex-col">
          <b className="text-[15.5px] break-words">{name}</b>
          <span className="muted text-[13px]">
            {session.isAdmin ? t("roleAdmin") : t("roleOperator")}
          </span>
        </span>
      </div>
      <section
        className="flex flex-col gap-[8px]"
        aria-labelledby="more-settings"
      >
        <h2 id="more-settings" className="eyebrow m-0 pl-[4px]">
          {t("settings")}
        </h2>
        <ul className="list card divide p-0">
          {tabs.map((tab) => (
            <li key={tab}>
              <Link to="/settings/$tab" params={{ tab }} className="more-row">
                <span>
                  {tab === "profile"
                    ? c("navProfile")
                    : tab === "sessions"
                      ? c("navSessions")
                      : t("people")}
                </span>
                <span className="ml-auto">
                  {tab === "people" && waiting > 0 && (
                    <span className="badge bg-[var(--sun2)] text-[var(--sun-ink)]">
                      {c("toApprove", {
                        count: waiting,
                      })}
                    </span>
                  )}
                </span>
                <ChevronIcon />
              </Link>
            </li>
          ))}
        </ul>
      </section>
      {failed && (
        <p role="alert" className="form-error m-0">
          {t("signOutFailed")}
        </p>
      )}
      <button
        type="button"
        className="btn btn-ghost btn-full more-signout"
        onClick={async () => {
          setFailed(false);
          if (!(await session.signOut())) setFailed(true);
        }}
      >
        {t("signOut")}
      </button>
    </div>
  );
}
