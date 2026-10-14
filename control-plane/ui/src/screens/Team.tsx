import { Link } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useLive, useLiveQuery } from "../app/live";
import { CoworkerMark, CoworkerStatus } from "../components/coworker";
import { Empty, ErrorState, Loading } from "../components/states";
import { useLocale } from "../i18n";
import { formatNumber } from "../i18n/format";
import { listWorkItems, loadSummary } from "../lib/api";
import type { WorkflowSummary, WorkItem } from "../lib/types";
import {
  coworkerState,
  groupOf,
  headlineItem,
  hoursAndMinutes,
  itemsByWorkflow,
  localized,
  periodFor,
} from "../work/model";
import { StatusLine } from "./workLines";

// Your team: one card per AI coworker with its character, role, live status,
// what it does on its own versus always asks you, and the week in numbers
// (HOR-459 Design → Team). Pages read the same for one or many coworkers.

export function Team() {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const { workflows, workflowsError, reloadWorkflows } = useLive();
  const week = periodFor("week");
  const items = useLiveQuery(() => listWorkItems(week), []);
  const summary = useLiveQuery(() => loadSummary(week), []);

  if (workflowsError) return <ErrorState onRetry={reloadWorkflows} />;
  if (!workflows || items.status === "loading") return <Loading />;
  if (items.status === "error") return <ErrorState onRetry={items.reload} />;

  const byWorkflow = itemsByWorkflow(workflows, items.data);
  const timeBack =
    summary.status === "ready" ? summary.data.timeBack : undefined;
  const handled = items.data.length;
  const total = timeBack?.configured
    ? hoursAndMinutes(timeBack.seconds)
    : undefined;

  return (
    <>
      <header className="page-head">
        <div>
          <h1 className="ib-hero ib-hero--page">{t("title")}</h1>
          <p className="lead">{t("lead", { count: workflows.length })}</p>
        </div>
        {workflows.length > 0 && (
          <dl className="m-0 flex gap-8">
            <Figure
              value={formatNumber(handled, locale)}
              label={t("handledThisWeek")}
            />
            {total && (
              <Figure
                value={t("hoursShort", {
                  hours: total.hours,
                  minutes: total.minutes,
                })}
                label={t("backThisWeek")}
              />
            )}
          </dl>
        )}
      </header>
      {workflows.length === 0 ? (
        <Empty>{t("empty")}</Empty>
      ) : (
        <ul className="list-none m-0 p-0 grid gap-5 desk:grid-cols-2">
          {workflows.map((workflow) => (
            <li key={workflow.key}>
              <CoworkerCard
                workflow={workflow}
                items={byWorkflow.get(workflow.key) ?? []}
                seconds={
                  timeBack?.byWorkflow.find(
                    (entry) => entry.workflowKey === workflow.key,
                  )?.seconds
                }
              />
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

function Figure({ value, label }: { value: string; label: string }) {
  return (
    <div className="ib-metric flex-col-reverse">
      <dt className="ib-metric__label">{label}</dt>
      <dd className="ib-metric__value m-0 text-2xl">{value}</dd>
    </div>
  );
}

function CoworkerCard({
  workflow,
  items,
  seconds,
}: {
  workflow: WorkflowSummary;
  items: WorkItem[];
  seconds?: number;
}) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const state = coworkerState(items);
  const headline = headlineItem(items);
  const done = items.filter((item) => item.state === "done").length;
  const needs = items.filter((item) => groupOf(item.state) === "needs").length;
  const back = seconds !== undefined ? hoursAndMinutes(seconds) : undefined;
  const onItsOwn = workflow.coworker.onItsOwn ?? [];
  return (
    <article
      className="ib-card ib-card--pad flex flex-col gap-4 h-full"
      aria-labelledby={`cw-${workflow.key}`}
    >
      <div className="flex items-start gap-3">
        <CoworkerMark
          coworker={workflow.coworker}
          size={48}
          state={state}
          decorative
        />
        <div className="min-w-0 flex-1">
          <h2 id={`cw-${workflow.key}`} className="m-0 text-lg font-semibold">
            {workflow.coworker.name}
          </h2>
          <p className="m-0 muted text-sm">
            {t("roleLine", { role: localized(workflow.coworker.role, locale) })}
          </p>
          <div className="mt-1">
            <CoworkerStatus state={state} />
          </div>
        </div>
        <Link
          to="/team/$key"
          params={{ key: workflow.key }}
          className="text-link"
        >
          {t("open")}
          <span className="sr-only"> {workflow.coworker.name}</span>
        </Link>
      </div>
      <p className="m-0 text-ink-2">
        {headline ? <StatusLine item={headline} /> : t("idle")}
      </p>
      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <h3 className="ib-eyebrow m-0 mb-1">{t("onItsOwn")}</h3>
          {onItsOwn.length === 0 ? (
            <p className="m-0 muted text-sm">{t("nothingOnItsOwn")}</p>
          ) : (
            <ul className="m-0 p-0 list-none text-sm flex flex-col gap-1">
              {onItsOwn.map((line, index) => (
                <li key={index}>{localized(line, locale)}</li>
              ))}
            </ul>
          )}
        </div>
        <div>
          <h3 className="ib-eyebrow m-0 mb-1">{t("alwaysAsks")}</h3>
          {workflow.alwaysAsks.length === 0 ? (
            <p className="m-0 muted text-sm">{t("asksNothing")}</p>
          ) : (
            <ul className="m-0 p-0 list-none text-sm flex flex-col gap-1">
              {workflow.alwaysAsks.map((ask) => (
                <li key={ask.nodeKey}>{localized(ask.title, locale)}</li>
              ))}
            </ul>
          )}
        </div>
      </div>
      <dl className="m-0 mt-auto pt-3 border-t border-line-faint flex flex-wrap gap-x-6 gap-y-2 text-sm">
        <div className="flex flex-row-reverse justify-end items-baseline gap-1.5">
          <dt className="muted">{t("handled")}</dt>
          <dd className="m-0 ib-num font-medium">
            {formatNumber(items.length, locale)}
          </dd>
        </div>
        <div className="flex flex-row-reverse justify-end items-baseline gap-1.5">
          <dt className="muted">{t("done")}</dt>
          <dd className="m-0 ib-num font-medium">
            {formatNumber(done, locale)}
          </dd>
        </div>
        <div className="flex flex-row-reverse justify-end items-baseline gap-1.5">
          <dt className="muted">{t("needsYou")}</dt>
          <dd className="m-0 ib-num font-medium">
            {formatNumber(needs, locale)}
          </dd>
        </div>
        {back && (
          <div className="flex flex-row-reverse justify-end items-baseline gap-1.5">
            <dt className="muted">{t("backEstimate")}</dt>
            <dd className="m-0 ib-num font-medium">
              {t("hoursShort", { hours: back.hours, minutes: back.minutes })}
            </dd>
          </div>
        )}
      </dl>
    </article>
  );
}
