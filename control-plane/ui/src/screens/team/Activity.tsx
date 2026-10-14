import { Link, useNavigate } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useLiveQuery } from "../../app/live";
import {
  Empty,
  ErrorState,
  Loading,
  StaleNotice,
} from "../../components/states";
import { useLocale } from "../../i18n";
import { formatNumber } from "../../i18n/format";
import { listConsequences, listWorkItems } from "../../lib/api";
import type { WorkflowSummary, WorkItem } from "../../lib/types";
import { failureCode, periodFor, type PeriodPreset } from "../../work/model";
import type { ActivityFilter } from "../Coworker";
import { StatusLine } from "../workLines";
import {
  activityAt,
  byDay,
  ChevronRight,
  itemRef,
  newestFirst,
  useDayLabel,
  useWhen,
  WorkBadge,
} from "./parts";

// Activity (boards XTCoworkerActivity / XTCoworkerFailed and their phone
// boards): the coworker's work in a period, filtered by state. The Failed
// filter switches to the failures view with why each one stopped and what
// can be done next. Who confirmed an item is not in the list projection, so
// the board's "Confirmed by" column is left out.

const filters: ActivityFilter[] = ["all", "needs", "done", "failed"];
const periods: PeriodPreset[] = ["today", "week", "month"];

function matches(item: WorkItem, filter: ActivityFilter): boolean {
  if (filter === "all") return true;
  if (filter === "needs") return item.state === "blocked";
  return item.state === filter;
}

export function Activity({
  workflow,
  filter,
  period,
}: {
  workflow: WorkflowSummary;
  filter: ActivityFilter;
  period: PeriodPreset;
}) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const navigate = useNavigate();
  const items = useLiveQuery(
    () => listWorkItems({ ...periodFor(period), workflowKey: workflow.key }),
    [workflow.key, period],
  );
  const go = (next: { filter?: ActivityFilter; period?: PeriodPreset }) =>
    void navigate({
      to: "/team/$key",
      params: { key: workflow.key },
      search: {
        tab: "activity",
        filter: next.filter ?? filter,
        period: next.period ?? period,
      },
    });

  return (
    <section className="cw-activity" aria-labelledby="cw-activity-title">
      <h2 id="cw-activity-title" className="sr-only">
        {t("tabs.activity")}
      </h2>
      <div className="flex flex-wrap items-center gap-[14px]">
        <div
          className="flex flex-wrap gap-[6px] desk:gap-2"
          role="group"
          aria-label={t("activity.filterLabel")}
        >
          {filters.map((value) => (
            <button
              key={value}
              type="button"
              className="cw-chip"
              aria-pressed={filter === value}
              onClick={() => go({ filter: value })}
            >
              {t(`filter.${value}`)}
              {items.status === "ready" && (
                <span className="mono opacity-70">
                  {formatNumber(
                    items.data.filter((item) => matches(item, value)).length,
                    locale,
                  )}
                </span>
              )}
            </button>
          ))}
        </div>
        <div className="ml-auto hidden desk:block">
          <div
            className="seg"
            role="group"
            aria-label={t("activity.periodLabel")}
          >
            {periods.map((value) => (
              <button
                key={value}
                type="button"
                aria-pressed={period === value}
                onClick={() => go({ period: value })}
              >
                {t(`period.${value}`)}
              </button>
            ))}
          </div>
        </div>
      </div>

      {items.status === "ready" && items.stale && (
        <StaleNotice onRetry={items.reload} />
      )}
      {items.status === "loading" && <Loading />}
      {items.status === "error" && <ErrorState onRetry={items.reload} />}
      {items.status === "ready" &&
        (filter === "failed" ? (
          <Failures
            items={items.data.filter((item) => item.state === "failed")}
            period={period}
          />
        ) : (
          <Rows
            items={items.data.filter((item) => matches(item, filter))}
            period={period}
          />
        ))}
    </section>
  );
}

/** The request as the boards set it: what it is, then who it came from. */
function Request({ item }: { item: WorkItem }) {
  return (
    <>
      <b className="text-[14px] font-semibold">{item.title}</b>
      {item.source.title && (
        <span className="muted text-[12.5px]"> · {item.source.title}</span>
      )}
    </>
  );
}

function RefLink({ item }: { item: WorkItem }) {
  return (
    <Link
      to="/work/$itemId"
      params={{ itemId: item.id }}
      className="cw-link mono whitespace-nowrap text-[12.5px]"
    >
      {itemRef(item.id)} <span className="sr-only">{item.title}</span>
    </Link>
  );
}

function Rows({ items, period }: { items: WorkItem[]; period: PeriodPreset }) {
  const { t } = useTranslation("team");
  const when = useWhen();
  const dayLabel = useDayLabel();
  if (items.length === 0) return <Empty>{t("activity.none")}</Empty>;
  return (
    <>
      <div className="card hidden overflow-hidden desk:block">
        <table className="cw-table">
          <thead>
            <tr className="eyebrow">
              <th scope="col">{t("col.time")}</th>
              <th scope="col">{t("col.item")}</th>
              <th scope="col">{t("col.request")}</th>
              <th scope="col">{t("col.where")}</th>
              <th scope="col">{t("col.status")}</th>
              <th scope="col">{t("col.confirmedBy")}</th>
            </tr>
          </thead>
          <tbody>
            {newestFirst(items).map((item) => (
              <tr key={item.id}>
                <td className="mono muted whitespace-nowrap text-[12.5px]">
                  {when(activityAt(item), period !== "today")}
                </td>
                <td>
                  <RefLink item={item} />
                </td>
                <td>
                  <Request item={item} />
                </td>
                <td className="text-[13.5px] text-[var(--ink2)]">
                  <StatusLine item={item} />
                </td>
                <td>
                  <WorkBadge item={item} />
                </td>
                <td className="text-[13.5px] text-[var(--ink2)]">
                  {item.answeredBy?.displayName ?? (
                    <span className="muted" aria-label={t("col.nobody")}>
                      —
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="desk:hidden">
        {byDay(items).map((group) => (
          <section key={group.day.getTime()} className="cw-day">
            <h3 className="eyebrow cw-day__label">{dayLabel(group.day)}</h3>
            <ul className="list card divide p-0">
              {group.items.map((item) => (
                <li key={item.id}>
                  <Link
                    to="/work/$itemId"
                    params={{ itemId: item.id }}
                    className="cw-row"
                  >
                    <span className="flex min-w-0 flex-col gap-px">
                      <span className="mono muted text-[11.5px]">
                        {when(activityAt(item))} · {itemRef(item.id)}
                      </span>
                      <b className="truncate text-[14px] font-semibold">
                        {item.title}
                      </b>
                      <span className="muted truncate text-[12.5px]">
                        <StatusLine item={item} />
                      </span>
                    </span>
                    <span className="ml-auto flex-none">
                      <WorkBadge item={item} dot={false} />
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          </section>
        ))}
      </div>
    </>
  );
}

/**
 * Failures in the period. A restarted item leaves the failed state, so every
 * row here is still waiting for a restart; a write whose outcome is unknown
 * is flagged and offered for review instead.
 */
function Failures({
  items,
  period,
}: {
  items: WorkItem[];
  period: PeriodPreset;
}) {
  const { t } = useTranslation("team");
  const { t: tw } = useTranslation("work");
  const when = useWhen();
  const dayLabel = useDayLabel();
  const ids = items.map((item) => item.id).join(",");
  const unknown = useLiveQuery(async () => {
    const flagged = new Set<string>();
    await Promise.all(
      items.map(async (item) => {
        const consequences = await listConsequences(item.id);
        if (consequences.some((c) => c.state === "outcome_unknown"))
          flagged.add(item.id);
      }),
    );
    return flagged;
  }, [ids]);
  const isUnknown = (item: WorkItem) =>
    unknown.status === "ready" && unknown.data.has(item.id);

  if (items.length === 0) return <Empty>{t("activity.none")}</Empty>;
  const unknownCount = items.filter(isUnknown).length;
  const summary = [
    t("failed.summary", { count: items.length }),
    unknownCount > 0 && t("failed.unknown", { count: unknownCount }),
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <>
      <div className="card hidden overflow-hidden desk:block">
        <table className="cw-table cw-table--failed">
          <thead>
            <tr className="eyebrow">
              <th scope="col">{t("col.when")}</th>
              <th scope="col">{t("col.item")}</th>
              <th scope="col">{t("col.request")}</th>
              <th scope="col">{t("col.why")}</th>
              <th scope="col">{t("col.after")}</th>
              <th scope="col">
                <span className="sr-only">{t("col.next")}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {newestFirst(items).map((item) => (
              <tr key={item.id}>
                <td className="mono muted whitespace-nowrap text-[12.5px]">
                  {when(activityAt(item), true)}
                </td>
                <td>
                  <RefLink item={item} />
                </td>
                <td className="text-[14px] font-semibold">{item.title}</td>
                <td className="text-[13.5px] text-[var(--ink2)]">
                  {tw(`failure.${failureCode(item)}`)}
                  {isUnknown(item) && (
                    <>
                      {" "}
                      <span className="badge bg-[var(--warn2)] text-[var(--warn-ink)]">
                        {t("failed.outcomeUnknown")}
                      </span>
                    </>
                  )}
                </td>
                <td>
                  <span className="badge bg-[var(--paper2)] text-[var(--ink2)]">
                    {t("failed.notRestarted")}
                  </span>
                </td>
                <td className="text-right">
                  <Link
                    to="/work/$itemId"
                    params={{ itemId: item.id }}
                    className="cw-link text-[13px] font-medium"
                  >
                    {isUnknown(item) ? t("failed.review") : t("failed.restart")}{" "}
                    <span className="sr-only">{item.title}</span>
                  </Link>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <p className="m-0 border-t border-[var(--line2)] px-[22px] py-3 text-[13px] text-[var(--ink2)]">
          {summary}
        </p>
      </div>

      <div className="desk:hidden">
        <p className="muted m-0 px-[2px] pt-[2px] text-[12.5px]">
          {t("failed.short", {
            count: items.length,
            period: t(`period.${period}`),
          })}
        </p>
        {byDay(items).map((group) => (
          <section key={group.day.getTime()} className="cw-day">
            <h3 className="eyebrow cw-day__label">{dayLabel(group.day)}</h3>
            <ul className="list card divide p-0">
              {group.items.map((item) => (
                <li key={item.id}>
                  <Link
                    to="/work/$itemId"
                    params={{ itemId: item.id }}
                    className="cw-row py-[10px]"
                  >
                    <span className="flex min-w-0 flex-1 flex-col gap-[2px]">
                      <b className="text-[14px] font-semibold">{item.title}</b>
                      <span className="muted text-[12.5px]">
                        {tw(`failure.${failureCode(item)}`)}
                      </span>
                      <span className="flex flex-wrap gap-[6px]">
                        <span className="badge bg-[var(--paper2)] text-[var(--ink2)]">
                          {t("failed.notRestarted")}
                        </span>
                        {isUnknown(item) && (
                          <span className="badge bg-[var(--warn2)] text-[var(--warn-ink)]">
                            {t("failed.outcomeUnknown")}
                          </span>
                        )}
                      </span>
                    </span>
                    <ChevronRight size={15} />
                  </Link>
                </li>
              ))}
            </ul>
          </section>
        ))}
      </div>
    </>
  );
}
