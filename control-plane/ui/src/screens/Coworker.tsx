import { Link, useNavigate } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useLive, useLiveQuery } from "../app/live";
import { CoworkerMark, CoworkerStatus } from "../components/coworker";
import { Segmented } from "../components/segmented";
import { Empty, ErrorState, Loading, NotFound } from "../components/states";
import { StateBadge } from "../components/work-state";
import { useLocale } from "../i18n";
import { formatShortDate, formatTime, formatNumber } from "../i18n/format";
import { listWorkItems } from "../lib/api";
import type { WorkflowSummary, WorkItem } from "../lib/types";
import {
  coworkerState,
  failureCode,
  groupFailures,
  groupOf,
  headlineItem,
  localized,
  periodFor,
  startOfDay,
  type PeriodPreset,
} from "../work/model";
import { StatusLine } from "./workLines";

// Coworker detail (HOR-459 Design → Coworker): How it works, What it can do,
// Activity and Failed this month, taken from the workflow definition
// (DES-HOR-459-01) and the coworker's live work.

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

  const tabs: Array<{ tab: CoworkerTab; label: string }> = [
    { tab: "how", label: t("tabs.how") },
    { tab: "can", label: t("tabs.can") },
    { tab: "activity", label: t("tabs.activity") },
    { tab: "failed", label: t("tabs.failed") },
  ];

  return (
    <>
      <Link to="/team" className="text-link">
        {t("backToTeam")}
      </Link>
      <header className="mt-4 mb-6 flex items-start gap-4">
        <CoworkerMark
          coworker={workflow.coworker}
          size={64}
          state={coworkerState(items)}
          decorative
        />
        <div className="min-w-0">
          <h1 className="ib-hero ib-hero--page">{workflow.coworker.name}</h1>
          <p className="lead mt-2">
            {localized(workflow.coworker.role, locale)} ·{" "}
            {localized(workflow.coworker.summary, locale)}
          </p>
          <div className="mt-2 flex flex-wrap items-center gap-3">
            <CoworkerStatus state={coworkerState(items)} />
            {headlineItem(items) && (
              <span className="text-sm text-ink-2">
                <StatusLine item={headlineItem(items)!} />
              </span>
            )}
          </div>
        </div>
      </header>

      <nav aria-label={t("tabsLabel")} className="settings-tabs">
        {tabs.map((entry) => (
          <Link
            key={entry.tab}
            to="/team/$key"
            params={{ key: workflowKey }}
            search={{ tab: entry.tab }}
            aria-current={tab === entry.tab ? "page" : undefined}
          >
            {entry.label}
            {entry.tab === "can" && (
              <span className="count-pill">{workflow.alwaysAsks.length}</span>
            )}
          </Link>
        ))}
      </nav>

      {week.status === "ready" && <WeekNumbers items={items} />}

      {tab === "how" && <HowItWorks workflow={workflow} items={items} />}
      {tab === "can" && <WhatItCanDo workflow={workflow} />}
      {tab === "activity" && (
        <Activity workflow={workflow} filter={filter} period={period} />
      )}
      {tab === "failed" && <FailedThisMonth workflow={workflow} />}
    </>
  );
}

function WeekNumbers({ items }: { items: WorkItem[] }) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const count = (predicate: (item: WorkItem) => boolean) =>
    formatNumber(items.filter(predicate).length, locale);
  return (
    <section aria-label={t("thisWeek")} className="mb-8">
      <h2 className="ib-eyebrow m-0 mb-2">{t("thisWeek")}</h2>
      <dl className="m-0 flex flex-wrap gap-x-10 gap-y-3">
        {[
          [count(() => true), t("handled")],
          [count((item) => item.state === "done"), t("done")],
          [count((item) => groupOf(item.state) === "needs"), t("needsYou")],
          [count((item) => item.state === "failed"), t("failedCount")],
        ].map(([value, label]) => (
          <div key={label} className="ib-metric flex-col-reverse">
            <dt className="ib-metric__label">{label}</dt>
            <dd className="ib-metric__value m-0 text-2xl">{value}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

/** One continuous process stream with live counts per step. */
function HowItWorks({
  workflow,
  items,
}: {
  workflow: WorkflowSummary;
  items: WorkItem[];
}) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const active = items.filter(
    (item) => groupOf(item.state) !== "done" && item.state !== "failed",
  );
  return (
    <section aria-labelledby="how-title">
      <h2 id="how-title" className="sr-only">
        {t("tabs.how")}
      </h2>
      <ol className="process">
        {workflow.steps.map((step) => {
          const here = active.filter(
            (item) => item.currentStep?.key === step.key,
          );
          const needs = here.filter((item) => item.state === "blocked").length;
          const waiting = here.filter(
            (item) => groupOf(item.state) === "waiting",
          ).length;
          const working = here.length - needs - waiting;
          return (
            <li key={step.key}>
              <span
                className={`ib-badge ${step.kind === "human_gate" ? "ib-badge--warm" : step.kind === "wait" ? "ib-badge--neutral" : "ib-badge--accent"}`}
              >
                {step.kind === "human_gate"
                  ? t("kind.you")
                  : step.kind === "wait"
                    ? t("kind.waits")
                    : workflow.coworker.name}
              </span>
              <span className="font-medium">
                {localized(step.label, locale)}
              </span>
              <span className="text-sm muted ib-num text-right">
                {[
                  working > 0 && t("live.working", { count: working }),
                  waiting > 0 && t("live.waiting", { count: waiting }),
                  needs > 0 && t("live.needs", { count: needs }),
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </span>
            </li>
          );
        })}
      </ol>
      <p className="mt-6 muted">
        {t("changeHow", { name: workflow.coworker.name })}
      </p>
    </section>
  );
}

function WhatItCanDo({ workflow }: { workflow: WorkflowSummary }) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const onItsOwn = workflow.coworker.onItsOwn ?? [];
  const connections = workflow.coworker.connections ?? [];
  return (
    <div className="grid gap-6 desk:grid-cols-2">
      <section className="ib-card ib-card--pad" aria-labelledby="own-title">
        <h2 id="own-title" className="section-title mb-3">
          {t("onItsOwn")}
        </h2>
        {onItsOwn.length === 0 ? (
          <Empty>{t("nothingOnItsOwn")}</Empty>
        ) : (
          <ul className="m-0 p-0 list-none flex flex-col gap-2">
            {onItsOwn.map((line, index) => (
              <li key={index}>{localized(line, locale)}</li>
            ))}
          </ul>
        )}
      </section>
      <section className="ib-card ib-card--pad" aria-labelledby="asks-title">
        <h2 id="asks-title" className="section-title mb-3">
          {t("alwaysAsksFirst")}
        </h2>
        {workflow.alwaysAsks.length === 0 ? (
          <Empty>{t("asksNothing")}</Empty>
        ) : (
          <ul className="m-0 p-0 list-none flex flex-col gap-2">
            {workflow.alwaysAsks.map((ask) => (
              <li
                key={ask.nodeKey}
                className="flex items-center justify-between gap-3"
              >
                <span>{localized(ask.title, locale)}</span>
                <span className="ib-badge ib-badge--warm">
                  {t("youConfirm")}
                </span>
              </li>
            ))}
          </ul>
        )}
        <p className="m-0 mt-4 text-sm muted">{t("anythingElseWaits")}</p>
      </section>
      <section
        className="ib-card ib-card--pad desk:col-span-2"
        aria-labelledby="conn-title"
      >
        <h2 id="conn-title" className="section-title mb-3">
          {t("connections")}
        </h2>
        {connections.length === 0 ? (
          <Empty>{t("noConnections")}</Empty>
        ) : (
          <ul className="m-0 p-0 list-none flex flex-col gap-2">
            {connections.map((line, index) => (
              <li key={index}>{localized(line, locale)}</li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}

const filters: ActivityFilter[] = ["all", "needs", "done", "failed"];
function matches(item: WorkItem, filter: ActivityFilter): boolean {
  if (filter === "all") return true;
  if (filter === "needs") return item.state === "blocked";
  return item.state === filter;
}

function Activity({
  workflow,
  filter,
  period,
}: {
  workflow: WorkflowSummary;
  filter: ActivityFilter;
  period: PeriodPreset;
}) {
  const { t } = useTranslation("team");
  const navigate = useNavigate();
  const items = useLiveQuery(
    () => listWorkItems({ ...periodFor(period), workflowKey: workflow.key }),
    [workflow.key, period],
  );
  return (
    <section aria-labelledby="activity-title">
      <h2 id="activity-title" className="sr-only">
        {t("tabs.activity")}
      </h2>
      <div className="flex flex-wrap items-center justify-between gap-3 mb-4">
        <div className="ib-seg" role="group" aria-label={t("filterLabel")}>
          {filters.map((value) => (
            <button
              key={value}
              type="button"
              aria-pressed={filter === value}
              onClick={() =>
                void navigate({
                  to: "/team/$key",
                  params: { key: workflow.key },
                  search: { tab: "activity", filter: value, period },
                })
              }
            >
              {t(`filter.${value}`)}
              {items.status === "ready" && (
                <span className="ib-num ml-1.5 muted">
                  {items.data.filter((item) => matches(item, value)).length}
                </span>
              )}
            </button>
          ))}
        </div>
        <Segmented
          label={t("periodLabel")}
          value={period}
          options={(["today", "week", "month"] as const).map((value) => ({
            value,
            label: t(`period.${value}`),
          }))}
          onChange={(value) =>
            void navigate({
              to: "/team/$key",
              params: { key: workflow.key },
              search: { tab: "activity", filter, period: value },
            })
          }
        />
      </div>
      {items.status === "loading" && <Loading />}
      {items.status === "error" && <ErrorState onRetry={items.reload} />}
      {items.status === "ready" && (
        <WorkTable items={items.data.filter((item) => matches(item, filter))} />
      )}
    </section>
  );
}

function WorkTable({
  items,
  failed = false,
}: {
  items: WorkItem[];
  failed?: boolean;
}) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  if (items.length === 0) return <Empty>{t("noActivity")}</Empty>;
  const today = startOfDay(new Date());
  return (
    <div className="overflow-x-auto ib-card">
      <table className="table">
        <thead>
          <tr>
            <th scope="col">{t("col.time")}</th>
            <th scope="col">{t("col.request")}</th>
            <th scope="col">{failed ? t("col.why") : t("col.where")}</th>
            <th scope="col">{t("col.status")}</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => {
            const at = item.finishedAt ?? item.createdAt;
            return (
              <tr key={item.id}>
                <td className="ib-num muted whitespace-nowrap">
                  {new Date(at) >= today
                    ? formatTime(at, locale)
                    : `${formatShortDate(at, locale)} · ${formatTime(at, locale)}`}
                </td>
                <td>
                  <Link
                    to="/work/$itemId"
                    params={{ itemId: item.id }}
                    className="text-link"
                  >
                    {item.title}
                  </Link>
                  <span className="block muted text-xs">
                    {item.source.title}
                  </span>
                </td>
                <td>
                  <StatusLine item={item} />
                </td>
                <td>
                  <StateBadge item={item} />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function FailedThisMonth({ workflow }: { workflow: WorkflowSummary }) {
  const { t } = useTranslation("team");
  const items = useLiveQuery(
    () =>
      listWorkItems({
        state: "failed",
        from: periodFor("month").from,
        workflowKey: workflow.key,
      }),
    [workflow.key],
  );
  if (items.status === "loading") return <Loading />;
  if (items.status === "error") return <ErrorState onRetry={items.reload} />;
  const groups = groupFailures(items.data);
  return (
    <section aria-labelledby="failed-month">
      <h2 id="failed-month" className="section-title mb-2">
        {t("failedMonth.title", { count: items.data.length })}
      </h2>
      {items.data.length > 0 && (
        <p className="m-0 mb-4 muted">
          {groups
            .map((group) =>
              t("failedMonth.byReason", {
                count: group.items.length,
                reason: t(`failure.${failureCode(group.items[0])}`, {
                  ns: "work",
                }),
              }),
            )
            .join(" · ")}
        </p>
      )}
      <WorkTable items={items.data} failed />
    </section>
  );
}
