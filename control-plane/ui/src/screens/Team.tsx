import { Link } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useLive, useLiveQuery } from "../app/live";
import { CoworkerMark } from "../components/coworker";
import { Empty, ErrorState, Loading, StaleNotice } from "../components/states";
import { TrustLine } from "../components/trust";
import { useLocale } from "../i18n";
import { formatNumber } from "../i18n/format";
import { listWorkItems, loadSummary } from "../lib/api";
import type { WorkflowSummary, WorkItem } from "../lib/types";
import {
  coworkerState,
  failureCode,
  groupOf,
  headlineItem,
  itemsByWorkflow,
  localized,
  periodFor,
} from "../work/model";
import { StatusLine } from "./workLines";
import {
  CheckMark,
  LockMark,
  RestartMark,
  StateDot,
  useHoursBack,
} from "./team/parts";

// Your team (boards XTTeam / XTPhoneTeam): one card per AI coworker with its
// character, role, live status, what it does on its own versus always asks
// you, and the week in numbers. Pages read the same for one or many.

export function Team() {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const hours = useHoursBack();
  const { workflows, workflowsError, reloadWorkflows } = useLive();
  const items = useLiveQuery(() => listWorkItems(periodFor("week")), []);
  const summary = useLiveQuery(() => loadSummary(periodFor("week")), []);

  if (workflowsError) return <ErrorState onRetry={reloadWorkflows} />;
  if (!workflows || items.status === "loading") return <Loading />;
  if (items.status === "error") return <ErrorState onRetry={items.reload} />;

  const byWorkflow = itemsByWorkflow(workflows, items.data);
  const timeBack =
    summary.status === "ready" && summary.data.timeBack.configured
      ? summary.data.timeBack
      : undefined;
  const count = workflows.length;
  const word =
    count >= 1 && count <= 9
      ? t(`countWord.${count as 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9}`)
      : formatNumber(count, locale);

  return (
    <div className="team-page">
      {items.stale && <StaleNotice onRetry={items.reload} />}
      <div className="flex flex-col gap-3 desk:flex-row desk:items-end desk:gap-5">
        <div className="flex flex-col gap-[10px]">
          <h1 className="serif m-0 text-[30px] leading-[1.08] desk:text-[40px]">
            {t("title")}
          </h1>
          {count > 0 && (
            <p className="m-0 max-w-[720px] text-[14.5px] leading-[1.5] text-[var(--ink2)] desk:text-[16px]">
              <span className="desk:hidden">
                {t("leadShort", { count, word })}
              </span>
              <span className="hidden desk:inline">
                {t("lead", { count, word })}
              </span>
            </p>
          )}
        </div>
        {count > 0 && (
          <dl className="team-totals">
            <div>
              <dt>{t("handledThisWeek")}</dt>
              <dd>{formatNumber(items.data.length, locale)}</dd>
            </div>
            {timeBack && (
              <div>
                <dt>
                  <span className="desk:hidden">{t("backShort")}</span>
                  <span className="hidden desk:inline">
                    {t("backThisWeek")}
                  </span>
                </dt>
                <dd>{hours(timeBack.seconds)}</dd>
              </div>
            )}
          </dl>
        )}
      </div>

      {count === 0 ? (
        <Empty>{t("empty")}</Empty>
      ) : (
        <ul className="team-cards">
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

      {count > 0 && (
        <p className="m-0 flex items-center gap-[7px] text-[12.5px] text-[var(--ink3)] desk:hidden">
          <LockMark size={13} width={2} stroke="var(--ink3)" />
          {t("asksBeforeCustomer")}
        </p>
      )}
      <footer className="team-footer">
        <TrustLine />
      </footer>
    </div>
  );
}

/** The newest failure, shown when the coworker's state is failed. */
function latestFailure(items: WorkItem[]): WorkItem | undefined {
  return items
    .filter((item) => item.state === "failed")
    .sort(
      (a, b) =>
        new Date(b.finishedAt ?? b.updatedAt).getTime() -
        new Date(a.finishedAt ?? a.updatedAt).getTime(),
    )[0];
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
  const { t: tw } = useTranslation("work");
  const { t: ta } = useTranslation("app");
  const locale = useLocale();
  const hours = useHoursBack();
  const state = coworkerState(items);
  const failure = state === "failed" ? latestFailure(items) : undefined;
  const headline = headlineItem(items);
  const role = localized(workflow.coworker.role, locale);
  const onItsOwn = workflow.coworker.onItsOwn ?? [];
  const done = items.filter((item) => item.state === "done").length;
  const needs = items.filter((item) => groupOf(item.state) === "needs").length;
  const figures: Array<[string, string]> = [
    [formatNumber(items.length, locale), t("handled")],
    [formatNumber(done, locale), t("done")],
    seconds !== undefined
      ? [hours(seconds), t("backShort")]
      : [formatNumber(needs, locale), t("needsYou")],
  ];
  const statusText = failure ? (
    t("failedTitle", { title: failure.title })
  ) : headline ? (
    <StatusLine item={headline} />
  ) : (
    t("idle")
  );
  const id = `cw-${workflow.key.replace(/[^\w-]/g, "-")}`;

  return (
    <article className="team-card" aria-labelledby={id}>
      <div className="team-card__head">
        <span className="team-card__mark">
          <CoworkerMark
            coworker={workflow.coworker}
            size={44}
            state={state}
            decorative
          />
        </span>
        <div className="flex min-w-0 flex-col gap-[2px]">
          <span className="flex flex-wrap items-center gap-x-[7px]">
            <h2
              id={id}
              className="m-0 text-[15px] font-semibold desk:text-[19px]"
            >
              {workflow.coworker.name}
            </h2>
            <span className="muted text-[12.5px] desk:hidden">{role}</span>
            <span
              className={`flex items-center gap-[7px] text-[12.5px] desk:hidden ${state === "failed" ? "text-[var(--crit-ink)]" : "text-[var(--ink3)]"}`}
            >
              <StateDot state={state} />
              {ta(`coworkerState.${state}`)}
            </span>
          </span>
          <span className="muted hidden text-[13px] desk:block">
            {t("roleLine", { role })}
          </span>
          <span className="text-[13.5px] leading-[1.4] text-[var(--ink2)] desk:hidden">
            {statusText}
          </span>
        </div>
        <Link
          to="/team/$key"
          params={{ key: workflow.key }}
          className="team-card__open"
        >
          {t("open")} <span className="sr-only">{workflow.coworker.name}</span>
        </Link>
      </div>

      {failure ? (
        <div className="team-card__failed">
          <p className="m-0">
            <b className="hidden desk:inline">
              {t("failedTitle", { title: failure.title })}{" "}
            </b>
            {tw(`failureHelp.${failureCode(failure)}`)}
          </p>
          <span className="flex flex-wrap gap-2">
            <Link
              to="/work/$itemId"
              params={{ itemId: failure.id }}
              className="btn btn-ink btn-sm team-card__restart"
            >
              <RestartMark size={15} />
              {t("restart")}
            </Link>
            <span className="hidden desk:inline-flex">
              <Link
                to="/work/$itemId"
                params={{ itemId: failure.id }}
                className="btn btn-ghost btn-sm"
              >
                {t("whatHappened")}
              </Link>
            </span>
          </span>
        </div>
      ) : (
        <p className="team-card__status">
          <StateDot state={state} />
          <span>{statusText}</span>
        </p>
      )}

      <div className="hidden grid-cols-2 gap-4 desk:grid">
        <div className="flex flex-col gap-2">
          <h3 className="eyebrow m-0">{t("onItsOwn")}</h3>
          {onItsOwn.length === 0 ? (
            <p className="muted m-0 text-[13.5px]">{t("nothingOnItsOwn")}</p>
          ) : (
            <ul className="list flex flex-col gap-[7px]">
              {onItsOwn.map((line, index) => (
                <li key={index} className="team-rule">
                  <CheckMark className="mt-[2px]" />
                  <span>{localized(line, locale)}</span>
                </li>
              ))}
            </ul>
          )}
        </div>
        <div className="flex flex-col gap-2">
          <h3 className="eyebrow m-0">{t("alwaysAsks")}</h3>
          {workflow.alwaysAsks.length === 0 ? (
            <p className="muted m-0 text-[13.5px]">{t("asksNothing")}</p>
          ) : (
            <ul className="list flex flex-col gap-[7px]">
              {workflow.alwaysAsks.map((ask) => (
                <li key={ask.nodeKey} className="team-rule">
                  <LockMark className="mt-[2px]" />
                  <span>{localized(ask.title, locale)}</span>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>

      <dl className="team-card__figures">
        {figures.map(([value, label]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      <p className="muted m-0 text-[12.5px] desk:hidden">
        {t("thisWeekLead")}{" "}
        {figures.map(([value, label], index) => (
          <span key={label}>
            {index > 0 && " · "}
            <b className="mono font-medium text-[var(--ink)]">{value}</b>{" "}
            {label}
          </span>
        ))}
      </p>
    </article>
  );
}
