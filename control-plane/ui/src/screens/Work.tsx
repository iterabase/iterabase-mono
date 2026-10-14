import { Link, Outlet, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useLiveQuery } from "../app/live";
import { CoworkerMark } from "../components/coworker";
import { Segmented } from "../components/segmented";
import { Empty, ErrorState, Loading, StaleNotice } from "../components/states";
import { listConsequences, listWorkItems } from "../lib/api";
import type { WorkItem } from "../lib/types";
import {
  boardGroups,
  daysAgo,
  failureCode,
  groupFailures,
  groupItems,
  periodFor,
  type Group,
  type PeriodPreset,
} from "../work/model";
import { SourceLine, StatusLine } from "./workLines";

// Work: everything the team is doing, waiting on, or needs from you, grouped
// Working / Waiting / Needs you / Done, with the Failed strip above (HOR-459
// Design → Work). The work item opens in a side sheet over this board.

const SHOWN = 4;

export function Work({ period }: { period: PeriodPreset }) {
  const { t } = useTranslation("work");
  const navigate = useNavigate();
  const items = useLiveQuery(() => {
    const { from, to } = periodFor(period);
    return listWorkItems({ from, to });
  }, [period]);

  return (
    <>
      <header className="page-head">
        <div>
          <h1 className="ib-hero ib-hero--page">{t("work.title")}</h1>
          <p className="lead">{t("work.lead")}</p>
        </div>
        <div className="flex items-center gap-3">
          <Segmented
            label={t("work.period")}
            value={period}
            onChange={(value) =>
              void navigate({ to: "/work", search: { period: value } })
            }
            options={(["today", "week", "month"] as const).map((value) => ({
              value,
              label: t(`period.${value}`),
            }))}
          />
          {items.status === "ready" && (
            <span className="muted text-sm ib-num" role="status">
              {t("work.itemCount", {
                count: items.data.filter((item) => item.state !== "failed")
                  .length,
              })}
            </span>
          )}
        </div>
      </header>

      <FailedStrip />

      {items.status === "loading" && <Loading />}
      {items.status === "error" && <ErrorState onRetry={items.reload} />}
      {items.status === "ready" && (
        <>
          {items.stale && <StaleNotice onRetry={items.reload} />}
          <Board items={items.data} />
        </>
      )}
      <Outlet />
    </>
  );
}

function Board({ items }: { items: WorkItem[] }) {
  const { t } = useTranslation("work");
  const groups = groupItems(items);
  return (
    <section className="board" aria-label={t("work.board")}>
      {boardGroups.map((group) => (
        <Column key={group} group={group} items={groups[group]} />
      ))}
    </section>
  );
}

function Column({
  group,
  items,
}: {
  group: Exclude<Group, "failed">;
  items: WorkItem[];
}) {
  const { t } = useTranslation("work");
  const [expanded, setExpanded] = useState(false);
  const shown = expanded ? items : items.slice(0, SHOWN);
  const headingId = `col-${group}`;
  return (
    <section
      className={`board-col ${group === "needs" ? "board-col--needs" : ""}`}
      aria-labelledby={headingId}
    >
      <div className="board-col__head">
        <h2 id={headingId} className="m-0 text-base font-semibold">
          {t(`group.${group}`)}
        </h2>
        <span className="ib-num muted text-sm">{items.length}</span>
        {group === "waiting" && (
          <span className="muted text-xs">{t("work.waitingNote")}</span>
        )}
      </div>
      {items.length === 0 && <Empty>{t(`work.empty.${group}`)}</Empty>}
      <ul className="list-none m-0 p-0 flex flex-col gap-2">
        {shown.map((item) => (
          <li key={item.id}>
            <WorkCard item={item} />
          </li>
        ))}
      </ul>
      {items.length > SHOWN && (
        <button
          type="button"
          className="text-link self-start px-1"
          aria-expanded={expanded}
          onClick={() => setExpanded((value) => !value)}
        >
          {expanded
            ? t("work.showFewer")
            : t("work.moreCount", { count: items.length - SHOWN })}
        </button>
      )}
    </section>
  );
}

export function WorkCard({ item }: { item: WorkItem }) {
  return (
    <Link
      to="/work/$itemId"
      params={{ itemId: item.id }}
      search={(previous) => previous}
      className="work-card"
    >
      <span className="work-card__meta">
        <CoworkerMark
          coworker={item.presentation.coworker}
          size={20}
          decorative
        />
        <span className="font-medium text-ink-2">
          {item.presentation.coworker.name}
        </span>
        <span>
          <SourceLine item={item} />
        </span>
      </span>
      <span className="work-card__title">{item.title}</span>
      <span className="work-card__line">
        <StatusLine item={item} />
      </span>
    </Link>
  );
}

/**
 * Real failures from the last 7 days, grouped by reason, with outcome unknown
 * flagged. Waiting, paused and cancelled work never appears here.
 */
function FailedStrip() {
  const { t } = useTranslation("work");
  const failed = useLiveQuery(async () => {
    const items = await listWorkItems({ state: "failed", from: daysAgo(7) });
    const month = await listWorkItems({
      state: "failed",
      from: periodFor("month").from,
    });
    const unknown = new Set<string>();
    await Promise.all(
      items.map(async (item) => {
        const consequences = await listConsequences(item.id);
        if (consequences.some((c) => c.state === "outcome_unknown"))
          unknown.add(item.id);
      }),
    );
    return { items, month: month.length, unknown };
  }, []);

  if (failed.status !== "ready" || failed.data.items.length === 0) return null;
  const { items, month, unknown } = failed.data;
  const workflows = new Set(items.map((item) => item.workflowKey));
  return (
    <section className="failed-strip" aria-labelledby="failed-title">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2
          id="failed-title"
          className="m-0 text-base font-semibold text-crit-ink"
        >
          {t("failed.title", { count: items.length })}
        </h2>
        {workflows.size === 1 ? (
          <Link
            to="/team/$key"
            params={{ key: [...workflows][0] }}
            search={{ tab: "failed" }}
            className="text-link"
          >
            {t("failed.allThisMonth", { count: month })}
          </Link>
        ) : (
          <Link to="/team" className="text-link">
            {t("failed.allThisMonth", { count: month })}
          </Link>
        )}
      </div>
      <p className="m-0 mt-1 muted text-sm">{t("failed.lead")}</p>
      <ul className="list-none m-0 mt-3 p-0 ib-divide">
        {groupFailures(items).map(({ code, items: grouped }) => {
          const anyUnknown = grouped.some((item) => unknown.has(item.id));
          return (
            <li
              key={code}
              className="py-3 flex flex-wrap items-center gap-x-4 gap-y-1"
            >
              <span className="font-semibold">{t(`failure.${code}`)}</span>
              {anyUnknown && (
                <span className="ib-badge ib-badge--warn">
                  {t("failed.outcomeUnknown")}
                </span>
              )}
              <span className="muted text-sm flex-1">
                {grouped.map((item, index) => (
                  <span key={item.id}>
                    {index > 0 && ", "}
                    <Link
                      to="/work/$itemId"
                      params={{ itemId: item.id }}
                      className="text-link"
                    >
                      {item.title}
                    </Link>
                  </span>
                ))}
              </span>
              <Link
                to="/work/$itemId"
                params={{ itemId: grouped[0].id }}
                className="text-link"
              >
                {anyUnknown ||
                failureCode(grouped[0]) === "interrupted_after_write"
                  ? t("failed.review")
                  : t("failed.reviewRestart")}
              </Link>
            </li>
          );
        })}
      </ul>
    </section>
  );
}
