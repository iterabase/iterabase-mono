import {
  Link,
  Outlet,
  useNavigate,
  useRouterState,
} from "@tanstack/react-router";
import {
  createContext,
  ReactNode,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { createPortal } from "react-dom";
import { useTranslation } from "react-i18next";
import { Logomark, SearchIcon } from "../components/brand";
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

// The app frame, ported from the "Delight, trusted" boards: a 68px desktop top
// bar (logo, pill navigation, search, avatar menu) and, on phone, a 56px
// header plus bottom tabs. No sidebar. Sofia and Reports are added by
// HOR-452 / HOR-658 (DES-HOR-459-03). Your turn is a focus mode without the
// frame. Screens own their padding, footers and phone header content.

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

/**
 * Phone header content for the current screen. A screen renders
 * <PhoneHeader> to replace the default logo-and-avatar header (a back link,
 * a centred title, actions); on desktop the phone header is hidden.
 */
const PhoneHeaderTarget = createContext<HTMLElement | null>(null);

export function PhoneHeader({ children }: { children: ReactNode }) {
  const target = useContext(PhoneHeaderTarget);
  return target ? createPortal(children, target) : null;
}

export function Shell() {
  const pathname = useRouterState({
    select: (state) => state.location.pathname,
  });
  if (pathname.startsWith("/your-turn"))
    return (
      <div className="xt min-h-dvh">
        <Outlet />
      </div>
    );
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
  const [phoneSlot, setPhoneSlot] = useState<HTMLElement | null>(null);
  const [customPhoneHeader, setCustomPhoneHeader] = useState(false);
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

  // The default phone header shows only while no screen fills the slot.
  useEffect(() => {
    if (!phoneSlot) return;
    const update = () => setCustomPhoneHeader(phoneSlot.childNodes.length > 0);
    update();
    const observer = new MutationObserver(update);
    observer.observe(phoneSlot, { childList: true });
    return () => observer.disconnect();
  }, [phoneSlot]);

  const signOut = async () => {
    setSignOutFailed(false);
    if (!(await session.signOut())) setSignOutFailed(true);
  };

  return (
    <PhoneHeaderTarget.Provider value={phoneSlot}>
      <div className="xt app-frame">
        <a href="#main" className="sr-only">
          {t("skipToContent")}
        </a>
        <header className="app-header">
          <div className="topbar">
            <Link to="/" className="topbar__brand" aria-label={t("homeLabel")}>
              <Logomark size={24} />
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
                    <span className="topnav__count mono">
                      <span aria-hidden="true">{needsYou}</span>
                      <span className="sr-only">
                        {t("needsYouCount", { count: needsYou })}
                      </span>
                    </span>
                  )}
                </Link>
              ))}
            </nav>
            <button
              type="button"
              className="topbar__search"
              onClick={() => setSearchOpen(true)}
            >
              <SearchIcon />
              {t("searchPlaceholder")}
              <kbd>{t("key.search")}</kbd>
            </button>
            <AccountMenu name={name} onSignOut={signOut} />
          </div>

          <div className="phonebar">
            <div
              ref={setPhoneSlot}
              className="phonebar__slot"
              hidden={!customPhoneHeader}
            />
            {!customPhoneHeader && (
              <>
                <Link to="/" aria-label={t("homeLabel")} className="row">
                  <Logomark size={28} />
                </Link>
                <span className="row phonebar__end">
                  <button
                    type="button"
                    className="phonebar__icon"
                    aria-label={t("search")}
                    onClick={() => setSearchOpen(true)}
                  >
                    <SearchIcon />
                  </button>
                  <Link
                    to="/more"
                    className="phonebar__avatar"
                    aria-label={t("accountLabel", { name })}
                  >
                    {initials(name)}
                  </Link>
                </span>
              </>
            )}
          </div>
        </header>

        {connection === "reconnecting" && (
          <div className="frame-notice" role="status">
            {t("reconnecting")}
          </div>
        )}
        {trialEnded && (
          <div className="frame-notice" role="status">
            {t("trialEnded")}
          </div>
        )}
        {signOutFailed && (
          <div className="frame-notice" role="alert">
            {t("signOutFailed")}
          </div>
        )}

        <main id="main" className="frame-main" tabIndex={-1}>
          <Outlet />
        </main>

        <nav aria-label={t("tabsNav")} className="phonetabs">
          {[...tabs, { tab: "more" as const, to: "/more" as const }].map(
            ({ tab, to }) => (
              <Link
                key={tab}
                to={to}
                aria-current={current === tab ? "page" : undefined}
              >
                <span className="phonetabs__bar" aria-hidden="true" />
                {t(`nav.${tab}`)}
                {tab === "work" && !!needsYou && (
                  <span className="phonetabs__count mono" aria-hidden="true">
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
    </PhoneHeaderTarget.Provider>
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
        className="topbar__avatar"
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

/** The current day and time for eyebrows, refreshed every minute. */
export function useNow(): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = setInterval(() => setNow(new Date()), 60_000);
    return () => clearInterval(timer);
  }, []);
  return now;
}

/** "Monday 13 October · 09:40" for screen eyebrows. */
export function useNowEyebrow(): string {
  const locale = useLocale();
  const now = useNow();
  return useMemo(
    () => `${formatLongDay(now, locale)} · ${formatTime(now, locale)}`,
    [now, locale],
  );
}
