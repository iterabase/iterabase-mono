import {
  Link,
  Outlet,
  useNavigate,
  useRouterState,
} from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Logomark, SearchIcon, TabIcon } from "../components/brand";
import { TrustLine } from "../components/trust";
import { Kbd } from "../components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "../components/ui/dropdown-menu";
import { formatLongDay, formatTime } from "../i18n/format";
import { useLocale } from "../i18n";
import { listWorkItems } from "../lib/api";
import { decisionsOf, initials } from "../work/model";
import { useLive, useLiveQuery } from "./live";
import { SearchDialog } from "./Search";
import { useSession } from "./session";

// The app frame (HOR-459 Decisions → Navigation): a desktop top bar and phone
// tabs, no sidebar. Sofia and Reports are added by HOR-452 / HOR-658
// (DES-HOR-459-03). Your turn is a focus mode without the frame.

type Tab = "today" | "work" | "team";
const tabs: Array<{ tab: Tab; to: "/" | "/work" | "/team" }> = [
  { tab: "today", to: "/" },
  { tab: "work", to: "/work" },
  { tab: "team", to: "/team" },
];

function activeTab(pathname: string): Tab | "more" | null {
  if (pathname === "/") return "today";
  if (pathname.startsWith("/work")) return "work";
  if (pathname.startsWith("/team")) return "team";
  if (pathname.startsWith("/more") || pathname.startsWith("/settings"))
    return "more";
  return null;
}

/** Needs-you count for the Work tab, from the live blocked list. */
function useNeedsYou(): number | undefined {
  const query = useLiveQuery(() => listWorkItems({ state: "blocked" }), []);
  return query.status === "ready" ? decisionsOf(query.data).length : undefined;
}

/** Paused-by-trial work means the trial ended (HOR-648 owns the trial). */
function useTrialEnded(): boolean {
  const query = useLiveQuery(() => listWorkItems({ state: "paused" }), []);
  return (
    query.status === "ready" &&
    query.data.some((item) => item.pause?.reason === "trial_ended")
  );
}

export function Shell() {
  const pathname = useRouterState({
    select: (state) => state.location.pathname,
  });
  if (pathname.startsWith("/your-turn")) return <Outlet />;
  return <Frame pathname={pathname} />;
}

function Frame({ pathname }: { pathname: string }) {
  const { t } = useTranslation("app");
  const session = useSession();
  const { connection } = useLive();
  const navigate = useNavigate();
  const needsYou = useNeedsYou();
  const trialEnded = useTrialEnded();
  const [searchOpen, setSearchOpen] = useState(false);
  const [signOutFailed, setSignOutFailed] = useState(false);
  const current = activeTab(pathname);
  const name = session.profile.displayName || session.profile.email;

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setSearchOpen(true);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  const signOut = async () => {
    setSignOutFailed(false);
    if (!(await session.signOut())) setSignOutFailed(true);
  };

  return (
    <div className="app">
      <a href="#main" className="sr-only">
        {t("skipToContent")}
      </a>
      <header className="app-header">
        <div className="topbar">
          <Link to="/" className="topbar__brand" aria-label={t("homeLabel")}>
            <Logomark />
            {t("brand")}
          </Link>
          <nav aria-label={t("mainNav")} className="topnav">
            {tabs.map(({ tab, to }) => (
              <Link
                key={tab}
                to={to}
                aria-current={current === tab ? "page" : undefined}
              >
                {t(`nav.${tab}`)}
                {tab === "work" && !!needsYou && (
                  <span className="count-pill">
                    <span aria-hidden="true">{needsYou}</span>
                    <span className="sr-only">
                      {t("needsYouCount", { count: needsYou })}
                    </span>
                  </span>
                )}
              </Link>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-3">
            <button
              type="button"
              className="search-trigger"
              onClick={() => setSearchOpen(true)}
            >
              <SearchIcon />
              {t("searchPlaceholder")}
              <Kbd>{t("key.search")}</Kbd>
            </button>
            <AccountMenu name={name} onSignOut={signOut} />
          </div>
        </div>

        <div className="phonebar">
          <Link to="/" aria-label={t("homeLabel")} className="inline-flex">
            <Logomark size={28} />
          </Link>
          <div className="ml-auto flex items-center gap-2">
            <button
              type="button"
              className="ib-iconbtn ib-iconbtn--md"
              aria-label={t("search")}
              onClick={() => setSearchOpen(true)}
            >
              <SearchIcon />
            </button>
            <Link
              to="/more"
              className="avatar avatar--sm no-underline"
              aria-label={t("accountLabel", { name })}
            >
              {initials(name)}
            </Link>
          </div>
        </div>
      </header>

      {connection === "reconnecting" && (
        <div className="notice-bar" role="status">
          {t("reconnecting")}
        </div>
      )}
      {trialEnded && (
        <div className="notice-bar" role="status">
          {t("trialEnded")}
        </div>
      )}
      {signOutFailed && (
        <div className="notice-bar" role="alert">
          {t("signOutFailed")}
        </div>
      )}

      <main id="main" className="app-main" tabIndex={-1}>
        <Outlet />
      </main>

      <footer className="trust-footer">
        <TrustLine />
      </footer>

      <nav aria-label={t("tabsNav")} className="phonetabs">
        {[...tabs, { tab: "more" as const, to: "/more" as const }].map(
          ({ tab, to }) => (
            <Link
              key={tab}
              to={to}
              aria-current={current === tab ? "page" : undefined}
            >
              <TabIcon name={tab} />
              {t(`nav.${tab}`)}
              {tab === "work" && !!needsYou && (
                <span className="count-pill" aria-hidden="true">
                  {needsYou}
                </span>
              )}
            </Link>
          ),
        )}
      </nav>

      <SearchDialog
        open={searchOpen}
        onOpenChange={setSearchOpen}
        onPick={(to) => {
          setSearchOpen(false);
          void navigate(to);
        }}
      />
    </div>
  );
}

function AccountMenu({
  name,
  onSignOut,
}: {
  name: string;
  onSignOut: () => void;
}) {
  const { t } = useTranslation("app");
  const session = useSession();
  const navigate = useNavigate();
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className="avatar"
        aria-label={t("accountLabel", { name })}
      >
        {initials(name)}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuLabel>
          {name} · {session.isAdmin ? t("roleAdmin") : t("roleOperator")}
        </DropdownMenuLabel>
        <DropdownMenuItem
          onSelect={() =>
            void navigate({ to: "/settings/$tab", params: { tab: "profile" } })
          }
        >
          {t("settings")}
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={onSignOut}>{t("signOut")}</DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** "Monday 13 October · 09:40" for screen eyebrows. */
export function useNowEyebrow(): string {
  const locale = useLocale();
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = setInterval(() => setNow(new Date()), 60_000);
    return () => clearInterval(timer);
  }, []);
  return useMemo(
    () => `${formatLongDay(now, locale)} · ${formatTime(now, locale)}`,
    [now, locale],
  );
}
