import { Link } from "@tanstack/react-router";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useSession } from "../app/session";
import { Button } from "../components/ui/button";
import { fixedT } from "../i18n";
import { initials } from "../work/model";
import { settingsTabs } from "./Settings";

// Phone "More" (HOR-664 tab): the person, the Settings tabs and Sign out.
// Reports joins with HOR-658.

export function More() {
  const { t } = useTranslation("app");
  const session = useSession();
  const c = fixedT(session.locale, "settings");
  const [failed, setFailed] = useState(false);
  const name = session.profile.displayName || session.profile.email;
  const tabs = settingsTabs.filter(
    (tab) => session.isAdmin || tab !== "people",
  );
  return (
    <>
      <h1 className="ib-hero">{t("nav.more")}</h1>
      <div className="mt-6 flex items-center gap-3">
        <span className="avatar avatar--quiet" aria-hidden="true">
          {initials(name)}
        </span>
        <div>
          <strong className="block">{name}</strong>
          <span className="muted text-sm">
            {session.isAdmin ? t("roleAdmin") : t("roleOperator")}
          </span>
        </div>
      </div>
      <h2 className="ib-eyebrow mt-8 mb-2">{t("settings")}</h2>
      <ul className="ib-card ib-divide list-none m-0 p-0 overflow-hidden">
        {tabs.map((tab) => (
          <li key={tab}>
            <Link to="/settings/$tab" params={{ tab }} className="row-link">
              {tab === "profile"
                ? c("navProfile")
                : tab === "sessions"
                  ? c("navSessions")
                  : t("people")}
            </Link>
          </li>
        ))}
      </ul>
      {failed && (
        <p role="alert" className="form-error mt-4">
          {t("signOutFailed")}
        </p>
      )}
      <Button
        className="mt-6"
        full
        onClick={async () => {
          setFailed(false);
          if (!(await session.signOut())) setFailed(true);
        }}
      >
        {t("signOut")}
      </Button>
    </>
  );
}
