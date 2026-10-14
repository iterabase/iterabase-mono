import { Link } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { PhoneHeader } from "../app/Shell";
import { useLive, useLiveQuery } from "../app/live";
import { CoworkerMark } from "../components/coworker";
import { ErrorState, Loading, NotFound } from "../components/states";
import { TrustLine } from "../components/trust";
import { useLocale } from "../i18n";
import { formatNumber } from "../i18n/format";
import { listWorkItems, loadSummary } from "../lib/api";
import type { WorkflowSummary, WorkItem } from "../lib/types";
import {
  coworkerState,
  groupOf,
  headlineItem,
  localized,
  periodFor,
  type PeriodPreset,
} from "../work/model";
import { StatusLine } from "./workLines";
import { Activity } from "./team/Activity";
import { HowItWorks } from "./team/HowItWorks";
import { ChevronLeft, StateDot, useHoursBack, WithEmails } from "./team/parts";
import { WhatItCanDo } from "./team/WhatItCanDo";

// AI coworker detail (boards XTCoworker*, XTPhoneCoworker*): How it works,
// What it can do and Activity (with its Failed view), taken from the
// workflow definition (DES-HOR-459-01) and the coworker's live work.

export type CoworkerTab = "how" | "can" | "activity" | "failed";
export type ActivityFilter = "all" | "needs" | "done" | "failed";

export function Coworker({
  workflowKey,
  tab,
  filter,
  period,
}: {
  workflowKey: string;
  tab: CoworkerTab;
  filter: ActivityFilter;
  period: PeriodPreset;
}) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const { workflows, workflowsError, reloadWorkflows } = useLive();
  const week = useLiveQuery(
    () => listWorkItems({ ...periodFor("week"), workflowKey }),
    [workflowKey],
  );

  if (workflowsError) return <ErrorState onRetry={reloadWorkflows} />;
  if (!workflows) return <Loading />;
  const workflow = workflows.find((candidate) => candidate.key === workflowKey);
  if (!workflow) return <NotFound />;
  const items = week.status === "ready" ? week.data : [];

  // The Failed view is Activity filtered to failures (boards
  // XTCoworkerFailed); the older ?tab=failed links land on it for the month.
  const section = tab === "failed" ? "activity" : tab;
  const activityFilter = tab === "failed" ? "failed" : filter;
  const activityPeriod = tab === "failed" ? "month" : period;

  return (
    <div className="cw-page">
      <PhoneHeader>
        <Link to="/team" className="phone-back cw-link">
          <ChevronLeft size={18} width={2.2} />
          {t("backTeam")}
        </Link>
        <span className="row ml-auto">
          <Link to="/work" className="cw-link text-[14px]">
            {t("itsWork")}
          </Link>
        </span>
      </PhoneHeader>

      <div className="flex flex-col gap-[10px] desk:gap-[18px]">
        <Link to="/team" className="cw-back">
          <ChevronLeft />
          {t("backToTeam")}
        </Link>
        <Identity
          workflow={workflow}
          items={items}
          ready={week.status === "ready"}
        />
        <div className="cw-tabs-row">
          <nav aria-label={t("tabsLabel")} className="cw-tabs">
            {(["how", "can", "activity"] as const).map((entry) => (
              <Link
                key={entry}
                to="/team/$key"
                params={{ key: workflowKey }}
                search={{ tab: entry }}
                aria-current={section === entry ? "page" : undefined}
              >
                {t(`tabs.${entry}`)}
                {entry === "can" && (
                  <span className="mono cw-tabs__count">
                    {formatNumber(
                      (workflow.coworker.onItsOwn ?? []).length +
                        workflow.alwaysAsks.length,
                      locale,
                    )}
                  </span>
                )}
              </Link>
            ))}
          </nav>
          {week.status === "ready" && (
            <WeekFigures workflowKey={workflowKey} items={items} />
          )}
        </div>
      </div>

      {section === "how" && <HowItWorks workflow={workflow} items={items} />}
      {section === "can" && <WhatItCanDo workflow={workflow} />}
      {section === "activity" && (
        <Activity
          workflow={workflow}
          filter={activityFilter}
          period={activityPeriod}
        />
      )}

      {section !== "activity" && (
        <footer className="team-footer">
          <TrustLine />
        </footer>
      )}
    </div>
  );
}

/** Mark, name, role and summary, and the live line of what it is doing. */
function Identity({
  workflow,
  items,
  ready,
}: {
  workflow: WorkflowSummary;
  items: WorkItem[];
  ready: boolean;
}) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const state = coworkerState(items);
  const headline = headlineItem(items);
  return (
    <div className="flex items-center gap-3 desk:gap-[18px]">
      <span className="cw-mark">
        <CoworkerMark
          coworker={workflow.coworker}
          size={56}
          state={ready ? state : undefined}
          decorative
        />
      </span>
      <div className="flex min-w-0 flex-col gap-[2px] desk:gap-1">
        <h1 className="serif m-0 text-[26px] leading-[1.05] desk:text-[34px]">
          {workflow.coworker.name}
        </h1>
        <span className="hidden text-[14.5px] text-[var(--ink2)] desk:block">
          {localized(workflow.coworker.role, locale)} ·{" "}
          <WithEmails text={localized(workflow.coworker.summary, locale)} />
        </span>
        {ready && (
          <span className="flex items-center gap-[7px] text-[12.5px] text-[var(--ink2)] desk:gap-2 desk:text-[13.5px] desk:text-[var(--ink)]">
            <StateDot state={state} />
            <span>{headline ? <StatusLine item={headline} /> : t("idle")}</span>
          </span>
        )}
      </div>
      <span className="ml-auto hidden flex-none desk:inline-flex">
        <Link to="/work" className="btn btn-secondary btn-sm">
          {t("seeWork")}
        </Link>
      </span>
    </div>
  );
}

/** The week in numbers: real counts, and the estimated hours back. */
function WeekFigures({
  workflowKey,
  items,
}: {
  workflowKey: string;
  items: WorkItem[];
}) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const hours = useHoursBack();
  const summary = useLiveQuery(() => loadSummary(periodFor("week")), []);
  const seconds =
    summary.status === "ready" && summary.data.timeBack.configured
      ? summary.data.timeBack.byWorkflow.find(
          (entry) => entry.workflowKey === workflowKey,
        )?.seconds
      : undefined;
  const count = (predicate: (item: WorkItem) => boolean) =>
    formatNumber(items.filter(predicate).length, locale);
  const figures: Array<[string, string]> = [
    [count(() => true), t("handled")],
    [count((item) => item.state === "done"), t("done")],
    [count((item) => groupOf(item.state) === "needs"), t("needsYou")],
    [count((item) => item.state === "failed"), t("failedCount")],
  ];
  if (seconds !== undefined) figures.push([hours(seconds), t("backShort")]);
  return (
    <div className="cw-week">
      <span className="eyebrow">{t("thisWeek")}</span>
      <dl className="m-0 flex flex-wrap items-baseline gap-x-[22px] gap-y-1">
        {figures.map(([value, label]) => (
          <div
            key={label}
            className="flex flex-row-reverse items-baseline gap-[6px]"
          >
            <dt className="muted text-[12.5px]">{label}</dt>
            <dd className="mono m-0 text-[15px] font-medium">{value}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
