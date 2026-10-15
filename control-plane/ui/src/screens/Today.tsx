import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { Trans, useTranslation } from "react-i18next";
import { useLive, useLiveQuery } from "../app/live";
import { SearchDialog } from "../app/Search";
import { useSession } from "../app/session";
import { PhoneHeader, useNow, useNowEyebrow } from "../app/Shell";
import { Logomark } from "../components/brand";
import { CoworkerMark, NoCoworkers } from "../components/coworker";
import { Empty, ErrorState, Loading, StaleNotice } from "../components/states";
import { useLocale } from "../i18n";
import { formatDay, formatNumber, formatTime } from "../i18n/format";
import { listWorkItems, loadSummary } from "../lib/api";
import type { WorkItem } from "../lib/types";
import {
  coworkerState,
  daysAgo,
  decisionsOf,
  firstName,
  greetingFor,
  groupItems,
  headlineItem,
  hoursAndMinutes,
  initials,
  itemsByWorkflow,
  localized,
  minutesBetween,
  periodFor,
  startOfDay,
} from "../work/model";
import { AllClear, useAllClear } from "./today/AllClear";
import {
  ArrowIcon,
  ChevronIcon,
  ClockIcon,
  DoneCheck,
  LockIcon,
  SearchIcon,
} from "./today/icons";
import { CoworkerRow, useIsDesk } from "./today/parts";
import { StatusLine } from "./workLines";

// Today, the home screen, ported from the XTToday and XTPhoneToday boards:
// the day in one sentence, the Your turn card, the team right now, what waits
// for you, what got done, and the status bar (time back · failed · trust).
// The day's sentence states counts the API has, never money (HOR-459).

export function Today() {
  const allClear = useAllClear();
  return allClear ? <AllClear state={allClear} /> : <TodayBoard />;
}

/** "About N minutes" for N decisions: a stated estimate, about 40 s each. */
export function aboutMinutes(decisions: number): number {
  return Math.max(1, Math.round((decisions * 40) / 60));
}

const DONE_SHOWN = 3;
/** The work list API returns at most this many rows. */
const WORK_LIST_LIMIT = 200;

function TodayBoard() {
  const { t } = useTranslation("today");
  const locale = useLocale();
  const session = useSession();
  const navigate = useNavigate();
  const desk = useIsDesk();
  const eyebrow = useNowEyebrow();
  const now = useNow();
  const { workflows } = useLive();
  // Since yesterday morning: active items plus what finished since then.
  const since = daysAgo(1, now);
  const items = useLiveQuery(
    () => listWorkItems({ from: since, to: periodFor("today", now).to }),
    [],
  );
  const today = useLiveQuery(() => loadSummary(periodFor("today")), []);
  // What needs the person is read on its own: the period list is capped and
  // newest first, so older open decisions could fall off it.
  const attention = useLiveQuery(
    () => listWorkItems({ state: "blocked", limit: WORK_LIST_LIMIT }),
    [],
  );

  const all = items.status === "ready" ? items.data : [];
  const decisions =
    attention.status === "ready" ? decisionsOf(attention.data) : [];
  // A full page may be cut short; the dashboard counts every open decision.
  const decisionCount =
    decisions.length >= WORK_LIST_LIMIT && today.status === "ready"
      ? Math.max(decisions.length, today.data.counts.blocked ?? 0)
      : decisions.length;
  const ready = items.status === "ready" && attention.status === "ready";
  const loadFailed = items.status === "error" || attention.status === "error";
  const loading =
    !loadFailed &&
    (items.status === "loading" || attention.status === "loading");
  const stale =
    (items.status === "ready" && items.stale) ||
    (attention.status === "ready" && attention.stale);
  const reload = () => {
    reload();
    attention.reload();
  };
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (
        event.key === "Enter" &&
        decisions.length > 0 &&
        event.target === document.body
      ) {
        event.preventDefault();
        void navigate({ to: "/your-turn" });
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [decisions.length, navigate]);

  const name = firstName(session.profile.displayName, session.profile.email);
  const greeting = t(`today.greeting.${greetingFor(now)}`, { name });
  const done = groupItems(all).done.filter(
    (item) =>
      item.finishedAt &&
      new Date(item.finishedAt).getTime() >= startOfDay(since).getTime(),
  );
  const failed = groupItems(all).failed.filter(
    (item) => minutesBetween(item.finishedAt ?? item.updatedAt) <= 24 * 60,
  );
  const about = t("aboutMinutes", { count: aboutMinutes(decisionCount) });
  const count = formatNumber(decisionCount, locale);

  return (
    <div className="today">
      <TodayPhoneHeader />
      {desk && <div className="glow today-glow" aria-hidden="true" />}
      <div className="today-grid">
        <section className="flex flex-col gap-[10px] desk:pt-[6px] min-w-0">
          {desk && <span className="eyebrow">{eyebrow}</span>}
          <h1 className="serif m-0 text-[32px] desk:text-[44px] leading-[1.08]">
            {greeting}
          </h1>
          {ready && (
            <p className="m-0 max-w-[720px] text-[15.5px] desk:text-[18px] leading-[1.5] text-[var(--ink2)]">
              <DaySentence done={done} />
              {desk && " "}
              {desk &&
                (decisions.length === 0 ? (
                  t("sentence.nothingWaiting")
                ) : (
                  <Trans
                    t={t}
                    i18nKey="sentence.decisions"
                    count={decisionCount}
                    values={{ about }}
                    components={{ b: <b className="text-[var(--ink)]" /> }}
                  />
                ))}
            </p>
          )}
        </section>

        {ready &&
          (desk ? (
            <section
              aria-labelledby="your-turn-title"
              className="card p-[22px] flex flex-col gap-[14px] self-end min-w-0"
            >
              <h2 id="your-turn-title" className="eyebrow m-0">
                {t("today.yourTurn")}
              </h2>
              {decisions.length === 0 ? (
                <p className="m-0 text-[15px] text-[var(--ink2)]">
                  {t("today.nothingNeedsYou")}
                </p>
              ) : (
                <>
                  <p className="row m-0 gap-3 items-baseline">
                    <span className="mono text-[40px] leading-none font-medium">
                      {count}
                    </span>
                    <span className="text-[15px] text-[var(--ink2)]">
                      {t("today.decisionsAbout", {
                        count: decisionCount,
                        about,
                      })}
                    </span>
                  </p>
                  <Link to="/your-turn" className="btn btn-ink btn-full">
                    {t("today.startFirst")}
                    <kbd aria-hidden="true">
                      {t("key.enter", { ns: "app" })}
                    </kbd>
                  </Link>
                  <span className="row gap-[7px] text-[12.5px] text-[var(--ink3)]">
                    <LockIcon />
                    <span>{t("today.exactFirst")}</span>
                  </span>
                </>
              )}
            </section>
          ) : (
            <section aria-labelledby="your-turn-title">
              <h2 id="your-turn-title" className="sr-only">
                {t("today.yourTurn")}
              </h2>
              {decisions.length === 0 ? (
                <p className="card m-0 p-4 text-[15px] text-[var(--ink2)]">
                  {t("today.nothingNeedsYou")}
                </p>
              ) : (
                <Link to="/your-turn" className="card turn-dark">
                  <span className="mono text-[34px] leading-none">{count}</span>
                  <span className="flex flex-col gap-[2px] min-w-0">
                    <b className="text-[15.5px]">
                      {t("today.decisionsWaiting", { count: decisionCount })}
                    </b>
                    <span className="text-[13px] text-[#CFC9BF]">{about}</span>
                  </span>
                  <span className="turn-dark__go">
                    <ArrowIcon />
                  </span>
                </Link>
              )}
            </section>
          ))}

        {loading && (
          <div className="desk:col-span-full">
            <Loading />
          </div>
        )}
        {loadFailed && (
          <div className="desk:col-span-full">
            <ErrorState onRetry={reload} />
          </div>
        )}
        {ready && (
          <>
            {stale && (
              <div className="desk:col-span-full">
                <StaleNotice onRetry={reload} />
              </div>
            )}
            <TeamNow items={all} desk={desk} />
            {desk && <WaitingForYou decisions={decisions} />}
            <DoneSinceYesterday done={done} desk={desk} />
          </>
        )}
        {workflows && workflows.length === 0 && ready && all.length === 0 && (
          <div className="desk:col-span-full">
            <NoCoworkers body={t("today.noCoworkers")} compact />
          </div>
        )}
      </div>

      {ready && (
        <StatusBar
          desk={desk}
          seconds={
            today.status === "ready" && today.data.timeBack.configured
              ? today.data.timeBack.seconds
              : undefined
          }
          failed={failed}
        />
      )}
    </div>
  );
}

/** XTPhoneToday's header: the logo, the short day and time, the avatar. */
function TodayPhoneHeader() {
  const { t } = useTranslation("app");
  const locale = useLocale();
  const session = useSession();
  const navigate = useNavigate();
  const now = useNow();
  const [searchOpen, setSearchOpen] = useState(false);
  const name = session.profile.displayName || session.profile.email;
  return (
    <PhoneHeader>
      <Link to="/" aria-label={t("homeLabel")} className="row">
        <Logomark size={24} />
      </Link>
      <span className="row ml-auto">
        <span className="eyebrow mr-[2px]">
          {`${formatDay(now, locale)} · ${formatTime(now, locale)}`}
        </span>
        {/* Search stays one tap away, as on every other phone screen. */}
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
      <SearchDialog
        open={searchOpen}
        onOpenChange={setSearchOpen}
        onPick={(to) => {
          setSearchOpen(false);
          void navigate(to);
        }}
      />
    </PhoneHeader>
  );
}

/**
 * The day in one sentence, from what the API counts: how many items finished
 * since yesterday and, when one AI coworker did them all, which one.
 */
function DaySentence({ done }: { done: WorkItem[] }) {
  const { t } = useTranslation("today");
  if (done.length === 0) return <>{t("sentence.nothingDone")}</>;
  const names = new Set(done.map((item) => item.presentation.coworker.name));
  const b = <b className="text-[var(--ink)]" />;
  return names.size === 1 ? (
    <Trans
      t={t}
      i18nKey="sentence.doneBy"
      count={done.length}
      values={{ name: [...names][0] }}
      components={{ b }}
    />
  ) : (
    <Trans
      t={t}
      i18nKey="sentence.doneTeam"
      count={done.length}
      components={{ b }}
    />
  );
}

function TeamNow({ items, desk }: { items: WorkItem[]; desk: boolean }) {
  const { t } = useTranslation("today");
  const { workflows } = useLive();
  const now = useNow();
  if (!workflows || workflows.length === 0) return null;
  const byWorkflow = itemsByWorkflow(workflows, items);
  const rows = workflows.map((workflow) => {
    const own = byWorkflow.get(workflow.key) ?? [];
    const state = coworkerState(own, now);
    // A failed coworker's line says what failed, as the boards show it.
    const headline =
      state === "failed"
        ? headlineItem(own.filter((item) => item.state === "failed"))
        : headlineItem(own);
    return {
      key: workflow.key,
      coworker: workflow.coworker,
      state,
      text: headline ? <StatusLine item={headline} /> : t("today.idle"),
    };
  });
  return (
    <section
      aria-labelledby="team-now"
      className="desk:col-span-full flex flex-col gap-2 desk:gap-3 min-w-0"
    >
      <div className="row gap-3 items-baseline">
        <h2
          id="team-now"
          className="m-0 text-[15px] desk:text-[17px] font-semibold"
        >
          {t("today.teamNow")}
        </h2>
        {desk && (
          <Link
            to="/team"
            className="text-[13.5px] text-[var(--ink3)] underline"
          >
            {t("today.openTeam")}
          </Link>
        )}
      </div>
      {desk ? (
        <ul className="list team-row">
          {rows.map((row) => (
            <li key={row.key} className="card p-4 min-w-0">
              <CoworkerRow
                coworker={row.coworker}
                state={row.state}
                text={row.text}
              />
            </li>
          ))}
        </ul>
      ) : (
        <ul className="list card divide p-0">
          {rows.map((row) => (
            <li key={row.key} className="px-[14px] py-3">
              <CoworkerRow
                coworker={row.coworker}
                state={row.state}
                text={row.text}
                size={32}
              />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function WaitingForYou({ decisions }: { decisions: WorkItem[] }) {
  const { t } = useTranslation("today");
  const locale = useLocale();
  return (
    <section
      aria-labelledby="waiting-for-you"
      className="card px-[22px] py-[18px] min-w-0"
    >
      <div className="row gap-[10px] items-baseline">
        <h2 id="waiting-for-you" className="m-0 text-[17px] font-semibold">
          {t("today.waitingForYou")}
        </h2>
        <span className="muted text-[13.5px]">{t("today.oldestFirst")}</span>
      </div>
      {decisions.length === 0 ? (
        <p className="m-0 mt-3 muted text-[14px]">
          {t("today.nothingWaiting")}
        </p>
      ) : (
        <ul className="list divide mt-[6px]">
          {decisions.map((item) => (
            <li key={item.id}>
              <Link
                to="/work/$itemId"
                params={{ itemId: item.id }}
                className="row today-link gap-[14px] py-[17px] px-1"
              >
                <CoworkerMark
                  coworker={item.presentation.coworker}
                  size={28}
                  decorative
                />
                <span className="flex flex-col gap-[2px] min-w-0">
                  <b className="text-[14.5px] font-semibold">
                    {localized(item.blocker?.title, locale) || item.title}
                  </b>
                  <span className="muted text-[13px]">
                    {item.source.title} · {item.title}
                  </span>
                </span>
                <span className="mono muted ml-auto w-[62px] flex-none text-right text-[12px]">
                  {t("minutesAgo", { count: minutesBetween(item.updatedAt) })}
                </span>
                <ChevronIcon />
              </Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function DoneSinceYesterday({
  done,
  desk,
}: {
  done: WorkItem[];
  desk: boolean;
}) {
  const { t } = useTranslation("today");
  const locale = useLocale();
  const shown = done.slice(0, DONE_SHOWN);
  const more = done.length > DONE_SHOWN && (
    <Link
      to="/work"
      search={{ period: "today" }}
      className="today-link text-[13.5px] text-[var(--ink3)] underline"
    >
      {t("today.seeAllDone", { count: done.length })}
    </Link>
  );
  const heading = (
    <h2
      id="done-since"
      className="m-0 text-[15px] desk:text-[17px] font-semibold"
    >
      {t("today.doneSince")}
    </h2>
  );
  if (desk)
    return (
      <section
        aria-labelledby="done-since"
        className="card px-[22px] py-[18px] min-w-0"
      >
        {heading}
        {done.length === 0 ? (
          <p className="m-0 mt-3 muted text-[14px]">{t("today.nothingDone")}</p>
        ) : (
          <ul className="list divide mt-[6px]">
            {shown.map((item) => (
              <li key={item.id}>
                <Link
                  to="/work/$itemId"
                  params={{ itemId: item.id }}
                  className="row today-link gap-[14px] py-[15px] px-1 items-start"
                >
                  <DoneCheck size={28} />
                  <span className="flex flex-col gap-[2px] min-w-0">
                    <b className="text-[14.5px] font-semibold">{item.title}</b>
                    <span className="muted text-[13px]">
                      {item.source.title}
                    </span>
                  </span>
                  <span className="mono muted ml-auto text-[12px] flex-none">
                    {item.finishedAt ? formatTime(item.finishedAt, locale) : ""}
                  </span>
                </Link>
              </li>
            ))}
            {more && <li className="pt-3 px-1">{more}</li>}
          </ul>
        )}
      </section>
    );
  return (
    <section aria-labelledby="done-since" className="flex flex-col gap-2">
      {heading}
      {done.length === 0 ? (
        <p className="card m-0 px-[14px] py-3 muted text-[14px]">
          {t("today.nothingDone")}
        </p>
      ) : (
        <ul className="list card divide p-0">
          {shown.map((item) => (
            <li key={item.id}>
              <Link
                to="/work/$itemId"
                params={{ itemId: item.id }}
                className="row today-link gap-3 px-[14px] py-[11px]"
              >
                <DoneCheck size={24} />
                <span className="text-[14px] leading-[1.35] min-w-0">
                  <b className="font-semibold">{item.title}</b>
                  <br />
                  <span className="muted text-[12.5px]">
                    {item.source.title}
                  </span>
                </span>
              </Link>
            </li>
          ))}
          {more && <li className="px-[14px] py-3">{more}</li>}
        </ul>
      )}
    </section>
  );
}

/** The bottom bar: time back (estimate) · failed pill · the trust line. */
function StatusBar({
  desk,
  seconds,
  failed,
}: {
  desk: boolean;
  seconds?: number;
  failed: WorkItem[];
}) {
  const { t } = useTranslation("today");
  const locale = useLocale();
  const timeBack =
    seconds === undefined
      ? null
      : (() => {
          const { hours, minutes } = hoursAndMinutes(seconds);
          return (
            <span className="row gap-[10px] desk:gap-[14px]">
              <ClockIcon size={desk ? 18 : 16} />
              <span className="text-[13.5px] desk:text-[14.5px]">
                <Trans
                  t={t}
                  i18nKey={desk ? "today.timeBack" : "today.timeBackShort"}
                  values={{
                    at: t("hoursMinutes", {
                      hours: formatNumber(hours, locale),
                      minutes: formatNumber(minutes, locale, {
                        minimumIntegerDigits: 2,
                      }),
                    }),
                  }}
                  components={{
                    b: <b className="mono font-semibold" />,
                    m: <span className="muted" />,
                  }}
                />
              </span>
            </span>
          );
        })();
  const failedLink = failed.length > 0 && (
    <Link
      to="/work"
      className="row today-link gap-[6px] text-[13.5px] text-[var(--crit-ink)] min-w-0"
    >
      <span className="badge bg-[var(--crit2)] text-[var(--crit-ink)]">
        {t("today.failedLine", { count: failed.length })}
      </span>
      <span className="underline truncate">{failed[0].title}</span>
    </Link>
  );
  if (!desk)
    return timeBack || failedLink ? (
      <div className="flex flex-col gap-2 px-[18px] pt-[14px] pb-6">
        {timeBack}
        {failedLink}
      </div>
    ) : null;
  return (
    <footer className="today-status">
      {timeBack}
      {failedLink && <span className="desk:ml-[18px]">{failedLink}</span>}
      <span className="row gap-[7px] ml-auto text-[12.5px] text-[var(--ink3)]">
        <LockIcon />
        <span>{t("trustLine", { ns: "app" })}</span>
      </span>
    </footer>
  );
}
