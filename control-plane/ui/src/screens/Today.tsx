import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect } from "react";
import { useTranslation } from "react-i18next";
import { useLive, useLiveQuery } from "../app/live";
import { useSession } from "../app/session";
import { useNowEyebrow } from "../app/Shell";
import { CoworkerLine, CoworkerMark } from "../components/coworker";
import { Empty, ErrorState, Loading, StaleNotice } from "../components/states";
import { Button, Kbd } from "../components/ui/button";
import { useLocale } from "../i18n";
import { formatTime } from "../i18n/format";
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
  itemsByWorkflow,
  minutesBetween,
  periodFor,
  startOfDay,
} from "../work/model";
import { StatusLine } from "./workLines";

// Today, the home screen: the day as a note from a colleague, the Your turn
// card, the team right now, what waits for you, what got done, time back and
// a quiet failed line (HOR-459 Design → Today).

export function Today() {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const session = useSession();
  const navigate = useNavigate();
  const eyebrow = useNowEyebrow();
  const { workflows } = useLive();
  const now = new Date();
  // Since yesterday morning: active items plus what finished since then.
  const since = daysAgo(1, now);
  const items = useLiveQuery(
    () => listWorkItems({ from: since, to: periodFor("today", now).to }),
    [],
  );
  const today = useLiveQuery(() => loadSummary(periodFor("today")), []);

  const decisions = items.status === "ready" ? decisionsOf(items.data) : [];
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

  return (
    <div className="flex flex-col gap-10">
      <header>
        <p className="ib-eyebrow m-0">{eyebrow}</p>
        <h1 className="ib-hero mt-3">{greeting}</h1>
        {items.status === "ready" && (
          <p className="lead">{daySentence(items.data, t)}</p>
        )}
      </header>

      {items.status === "loading" && <Loading />}
      {items.status === "error" && <ErrorState onRetry={items.reload} />}
      {items.status === "ready" && (
        <>
          {items.stale && <StaleNotice onRetry={items.reload} />}
          {yourTurnCard(decisions.length)}
          {teamNow(items.data)}
          {waitingForYou(decisions)}
          {doneSinceYesterday(items.data)}
          <div className="flex flex-col gap-2">
            {today.status === "ready" &&
              today.data.timeBack.configured &&
              timeBack(today.data.timeBack.seconds)}
            {failedLine(items.data)}
          </div>
        </>
      )}
      {workflows &&
        workflows.length === 0 &&
        items.status === "ready" &&
        items.data.length === 0 && <Empty>{t("today.noCoworkers")}</Empty>}
    </div>
  );

  function daySentence(all: WorkItem[], tt: typeof t): string {
    const groups = groupItems(all);
    const doneSince = groups.done.length;
    const n = decisionsOf(all).length;
    const done = tt("today.sentenceDone", { count: doneSince });
    const waiting =
      n === 0
        ? tt("today.sentenceNothingWaiting")
        : tt("today.sentenceDecisions", { count: n, minutes: aboutMinutes(n) });
    return `${done} ${waiting}`;
  }

  function yourTurnCard(count: number) {
    if (count === 0)
      return (
        <section
          className="ib-card ib-card--pad"
          aria-labelledby="your-turn-title"
        >
          <h2 id="your-turn-title" className="section-title">
            {t("today.yourTurn")}
          </h2>
          <p className="m-0 mt-1 muted">{t("today.nothingNeedsYou")}</p>
        </section>
      );
    return (
      <section
        className="ib-card ib-card--warm p-5 flex flex-wrap items-center justify-between gap-4"
        aria-labelledby="your-turn-title"
      >
        <div>
          <h2 id="your-turn-title" className="section-title">
            {t("today.yourTurn")}
          </h2>
          <p className="m-0 mt-1">
            <span className="ib-num text-2xl font-medium">{count}</span>{" "}
            {t("today.decisionsAbout", { count, minutes: aboutMinutes(count) })}
          </p>
          <p className="m-0 mt-1 text-sm text-warm-ink">
            {t("today.exactFirst")}
          </p>
        </div>
        <Button asChild variant="ink" size="lg">
          <Link to="/your-turn">
            {t("today.startFirst")}
            <Kbd>{t("key.enter", { ns: "app" })}</Kbd>
          </Link>
        </Button>
      </section>
    );
  }

  function teamNow(all: WorkItem[]) {
    if (!workflows || workflows.length === 0) return null;
    const byWorkflow = itemsByWorkflow(workflows, all);
    return (
      <section aria-labelledby="team-now">
        <div className="flex items-center justify-between mb-3">
          <h2 id="team-now" className="section-title">
            {t("today.teamNow")}
          </h2>
          <Link to="/team" className="text-link">
            {t("today.openTeam")}
          </Link>
        </div>
        <ul className="ib-card ib-divide list-none m-0 p-0">
          {workflows.map((workflow) => {
            const own = byWorkflow.get(workflow.key) ?? [];
            const headline = headlineItem(own);
            return (
              <li key={workflow.key} className="px-4 py-3">
                <CoworkerLine
                  coworker={workflow.coworker}
                  state={coworkerState(own, now)}
                  text={
                    headline ? <StatusLine item={headline} /> : t("today.idle")
                  }
                />
              </li>
            );
          })}
        </ul>
      </section>
    );
  }

  function waitingForYou(waiting: WorkItem[]) {
    return (
      <section aria-labelledby="waiting-for-you">
        <div className="flex items-baseline gap-3 mb-3">
          <h2 id="waiting-for-you" className="section-title">
            {t("today.waitingForYou")}
          </h2>
          <span className="muted text-sm">{t("today.oldestFirst")}</span>
        </div>
        {waiting.length === 0 ? (
          <Empty>{t("today.nothingWaiting")}</Empty>
        ) : (
          <ul className="ib-card ib-divide list-none m-0 p-0 overflow-hidden">
            {waiting.map((item) => (
              <li key={item.id}>
                <Link
                  to="/work/$itemId"
                  params={{ itemId: item.id }}
                  className="row-link"
                >
                  <CoworkerMark
                    coworker={item.presentation.coworker}
                    size={28}
                    decorative
                  />
                  <span className="min-w-0 flex-1">
                    <strong className="block">{item.title}</strong>
                    <span className="block muted text-sm">
                      <StatusLine item={item} />
                    </span>
                  </span>
                  <span className="ib-num muted text-sm">
                    {t("minutesAgo", { count: minutesBetween(item.updatedAt) })}
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        )}
      </section>
    );
  }

  function doneSinceYesterday(all: WorkItem[]) {
    const done = groupItems(all).done.filter(
      (item) =>
        item.finishedAt &&
        new Date(item.finishedAt).getTime() >= startOfDay(since).getTime(),
    );
    return (
      <section aria-labelledby="done-since">
        <h2 id="done-since" className="section-title mb-3">
          {t("today.doneSince")}
        </h2>
        {done.length === 0 ? (
          <Empty>{t("today.nothingDone")}</Empty>
        ) : (
          <ul className="ib-card ib-divide list-none m-0 p-0 overflow-hidden">
            {done.slice(0, 6).map((item) => (
              <li key={item.id}>
                <Link
                  to="/work/$itemId"
                  params={{ itemId: item.id }}
                  className="row-link"
                >
                  <CoworkerMark
                    coworker={item.presentation.coworker}
                    size={28}
                    state="done"
                    decorative
                  />
                  <span className="min-w-0 flex-1">
                    <strong className="block">{item.title}</strong>
                    <span className="block muted text-sm">
                      {item.source.title}
                    </span>
                  </span>
                  <span className="ib-num muted text-sm">
                    {item.finishedAt ? formatTime(item.finishedAt, locale) : ""}
                  </span>
                </Link>
              </li>
            ))}
            {done.length > 6 && (
              <li className="px-4 py-3">
                <Link
                  to="/work"
                  search={{ period: "today" }}
                  className="text-link"
                >
                  {t("today.seeAllDone", { count: done.length })}
                </Link>
              </li>
            )}
          </ul>
        )}
      </section>
    );
  }

  function timeBack(seconds: number) {
    const { hours, minutes } = hoursAndMinutes(seconds);
    return (
      <p className="m-0">
        {t("today.timeBack", {
          at: t("hoursMinutes", { hours, minutes }),
        })}
      </p>
    );
  }

  function failedLine(all: WorkItem[]) {
    const failed = groupItems(all).failed.filter(
      (item) => minutesBetween(item.finishedAt ?? item.updatedAt) <= 24 * 60,
    );
    if (failed.length === 0) return null;
    return (
      <p className="m-0 text-sm">
        <Link to="/work" className="text-link">
          {t("today.failedLine", { count: failed.length })}
        </Link>{" "}
        <span className="muted">{failed[0].title}</span>
      </p>
    );
  }
}

/** "About M minutes" for N decisions: a stated estimate, about 40 s each. */
export function aboutMinutes(decisions: number): number {
  return Math.max(1, Math.round((decisions * 40) / 60));
}
