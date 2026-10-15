import { Link, Outlet, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useLiveQuery } from "../app/live";
import { CoworkerMark } from "../components/coworker";
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
import { ChevronRight, LockIcon, WarnIcon } from "./work/icons";
import { useDesktop } from "./work/media";
import { AgeLabel, SourceLine, StatusLine } from "./workLines";

// Work (XTWork, XTWorkFailed, XTPhoneWork, XTPhoneWorkFailed): everything the
// team is doing, waiting on, or needs from you, in four columns on desktop and
// behind filter chips on phone, with the Failed strip above. The work item
// opens in a side sheet over this board (full screen on phone).

const SHOWN = 4;
type BoardGroup = Exclude<Group, "failed">;

export function Work({ period }: { period: PeriodPreset }) {
  const { t } = useTranslation("work");
  const navigate = useNavigate();
  const desktop = useDesktop();
  const items = useLiveQuery(() => {
    const { from, to } = periodFor(period);
    return listWorkItems({ from, to });
  }, [period]);

  return (
    <div className="work-page">
      <div className="flex flex-wrap items-end gap-x-[10px] gap-y-3 desk:gap-x-4">
        <div className="order-1 flex flex-col gap-[10px]">
          <h1 className="serif m-0 text-[28px] desk:text-[34px] leading-[1.08]">
            {t("work.title")}
          </h1>
          <p className="hidden desk:block m-0 max-w-[720px] text-[15px] leading-[1.5] text-[var(--ink2)]">
            {t("work.lead")}
          </p>
        </div>
        <div className="contents desk:flex desk:items-center desk:gap-[10px] desk:ml-auto desk:order-2">
          <div
            className="seg order-3 desk:order-none"
            role="group"
            aria-label={t("work.period")}
          >
            {(["today", "week", "month"] as const).map((value) => (
              <button
                key={value}
                type="button"
                aria-pressed={value === period}
                onClick={() =>
                  void navigate({ to: "/work", search: { period: value } })
                }
              >
                {t(`period.${value}`)}
              </button>
            ))}
          </div>
          <span
            className="order-2 ml-auto desk:order-none desk:ml-0 mono muted text-[12px] desk:text-[12.5px]"
            role="status"
          >
            {items.status === "ready" &&
              t("work.itemCount", {
                count: items.data.filter((item) => item.state !== "failed")
                  .length,
              })}
          </span>
        </div>
      </div>

      <FailedStrip desktop={desktop} />

      {items.status === "loading" && <Loading />}
      {items.status === "error" && <ErrorState onRetry={items.reload} />}
      {items.status === "ready" && (
        <>
          {items.stale && <StaleNotice onRetry={items.reload} />}
          {desktop ? (
            <Board items={items.data} period={period} />
          ) : (
            <PhoneBoard items={items.data} />
          )}
        </>
      )}

      {desktop ? (
        <footer className="mt-auto pt-6 pb-[22px]">
          <TrustNote text={t("work.trust")} />
        </footer>
      ) : (
        <TrustNote text={t("work.trustShort")} />
      )}
      <Outlet />
    </div>
  );
}

function TrustNote({ text }: { text: string }) {
  return (
    <p className="row m-0 gap-[7px] text-[12.5px] text-[var(--ink3)]">
      <LockIcon />
      <span>{text}</span>
    </p>
  );
}

function Board({ items, period }: { items: WorkItem[]; period: PeriodPreset }) {
  const { t } = useTranslation("work");
  const groups = groupItems(items);
  return (
    <section
      className="grid grid-cols-4 gap-[14px] items-start"
      aria-label={t("work.board")}
    >
      {boardGroups.map((group) => (
        <Column
          key={group}
          group={group}
          items={groups[group]}
          period={period}
        />
      ))}
    </section>
  );
}

const dotTone: Record<BoardGroup, string> = {
  working: "bg-[var(--ok)] live",
  waiting: "bg-[var(--ink3)]",
  needs: "bg-[var(--sun)]",
  done: "bg-[var(--ok)]",
};

function Column({
  group,
  items,
  period,
}: {
  group: BoardGroup;
  items: WorkItem[];
  period: PeriodPreset;
}) {
  const { t } = useTranslation("work");
  const [expanded, setExpanded] = useState(false);
  const shown = expanded ? items : items.slice(0, SHOWN);
  const headingId = `col-${group}`;
  const note =
    group === "waiting"
      ? t("work.waitingNote")
      : group === "done"
        ? t(`work.doneNote.${period}`)
        : "";
  return (
    <section
      className={`work-col ${group === "needs" ? "bg-[var(--sun2)]" : "bg-[var(--paper2)]"}`}
      aria-labelledby={headingId}
    >
      <div className="row gap-2 px-1 py-[2px]">
        <span className={`dot ${dotTone[group]}`} aria-hidden="true" />
        <h2 id={headingId} className="m-0 text-[14.5px] font-bold">
          {t(`group.${group}`)}
        </h2>
        <span className="mono muted text-[13px]">{items.length}</span>
        {note && (
          <span className="ml-auto muted text-[12.5px] text-right">{note}</span>
        )}
      </div>
      {items.length === 0 && <Empty>{t(`work.empty.${group}`)}</Empty>}
      {shown.length > 0 && (
        <ul className="list flex flex-col gap-[10px]">
          {shown.map((item) => (
            <li key={item.id}>
              <WorkCard item={item} />
            </li>
          ))}
        </ul>
      )}
      {items.length > SHOWN && (
        <button
          type="button"
          className="work-more"
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
      className="card work-card"
    >
      <span className="row gap-2 min-w-0">
        <CoworkerMark
          coworker={item.presentation.coworker}
          size={20}
          state={item.state === "in_progress" ? "working" : undefined}
          decorative
        />
        <span className="muted text-[12px] truncate">
          {item.presentation.coworker.name}
        </span>
        <span className="mono muted ml-auto text-[11.5px] whitespace-nowrap">
          <SourceLine item={item} />
        </span>
      </span>
      <b className="text-[14.5px] font-semibold leading-[1.3]">{item.title}</b>
      {item.state === "blocked" ? (
        <span className="work-ask">
          <StatusLine item={item} />
        </span>
      ) : (
        <span className="text-[13px] text-[var(--ink2)] leading-[1.4]">
          <StatusLine item={item} />
        </span>
      )}
    </Link>
  );
}

/** Phone: one group at a time behind filter chips (XTPhoneWork). */
function PhoneBoard({ items }: { items: WorkItem[] }) {
  const { t } = useTranslation("work");
  const groups = groupItems(items);
  const [picked, setPicked] = useState<BoardGroup | null>(null);
  const [expanded, setExpanded] = useState(false);
  const group =
    picked ??
    (["needs", "working", "waiting", "done"] as const).find(
      (candidate) => groups[candidate].length > 0,
    ) ??
    "needs";
  const list = groups[group];
  const shown = expanded ? list : list.slice(0, SHOWN * 2);
  return (
    <>
      <div className="work-chips" role="group" aria-label={t("work.board")}>
        {boardGroups.map((candidate) => (
          <button
            key={candidate}
            type="button"
            data-group={candidate}
            aria-pressed={candidate === group}
            onClick={() => {
              setPicked(candidate);
              setExpanded(false);
            }}
          >
            {t(`group.${candidate}`)}{" "}
            <span className="mono">{groups[candidate].length}</span>
          </button>
        ))}
      </div>
      <section aria-label={t(`group.${group}`)}>
        {list.length === 0 ? (
          <Empty>{t(`work.empty.${group}`)}</Empty>
        ) : (
          <ul className="list card divide p-0">
            {shown.map((item) => (
              <li key={item.id}>
                <Link
                  to="/work/$itemId"
                  params={{ itemId: item.id }}
                  search={(previous) => previous}
                  className="row gap-3 px-[14px] py-3 min-h-[44px] no-underline"
                >
                  <CoworkerMark
                    coworker={item.presentation.coworker}
                    size={28}
                    state={item.state === "in_progress" ? "working" : undefined}
                    decorative
                  />
                  <span className="flex flex-col gap-[2px] min-w-0">
                    <b className="text-[14.5px] font-semibold leading-[1.3]">
                      {item.title}
                    </b>
                    <span
                      className={`text-[12.5px] ${item.state === "blocked" ? "text-[var(--sun-ink)]" : "text-[var(--ink2)]"}`}
                    >
                      <StatusLine item={item} />
                    </span>
                  </span>
                  <span className="mono muted ml-auto text-[11.5px] whitespace-nowrap">
                    <AgeLabel at={item.createdAt} />
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        )}
        {(list.length > shown.length || expanded) && (
          <button
            type="button"
            className="work-more mt-2"
            aria-expanded={expanded}
            onClick={() => setExpanded((value) => !value)}
          >
            {expanded
              ? t("work.showFewer")
              : t("work.moreCount", { count: list.length - shown.length })}
          </button>
        )}
      </section>
    </>
  );
}

interface Failures {
  items: WorkItem[];
  month: number;
  unknown: Set<string>;
  /** Items with any action recorded outside Iterabase. */
  acted: Set<string>;
}

/**
 * Real failures from the last 7 days, grouped by reason, with outcome unknown
 * flagged. Waiting, paused and cancelled work never appears here. One failure
 * is a single bar (XTWork); several expand into rows (XTWorkFailed).
 */
function FailedStrip({ desktop }: { desktop: boolean }) {
  const failed = useLiveQuery(async (): Promise<Failures> => {
    const items = await listWorkItems({ state: "failed", from: daysAgo(7) });
    const month = await listWorkItems({
      state: "failed",
      from: periodFor("month").from,
    });
    const unknown = new Set<string>();
    const acted = new Set<string>();
    await Promise.all(
      items.map(async (item) => {
        const consequences = await listConsequences(item.id);
        if (consequences.length > 0) acted.add(item.id);
        if (consequences.some((c) => c.state === "outcome_unknown"))
          unknown.add(item.id);
      }),
    );
    return { items, month: month.length, unknown, acted };
  }, []);

  if (failed.status !== "ready" || failed.data.items.length === 0) return null;
  const data = failed.data;
  if (data.items.length === 1)
    return <SingleFailure data={data} desktop={desktop} />;
  return desktop ? <FailureList data={data} /> : <PhoneFailures data={data} />;
}

function SingleFailure({
  data,
  desktop,
}: {
  data: Failures;
  desktop: boolean;
}) {
  const { t } = useTranslation("work");
  const item = data.items[0];
  const unknown = data.unknown.has(item.id);
  const nothingSent = !data.acted.has(item.id);
  return (
    <section className="card failed-bar" aria-labelledby="failed-title">
      <h2
        id="failed-title"
        className="badge m-0 bg-[var(--crit2)] text-[var(--crit-ink)]"
      >
        {t("failed.title", { count: 1 })}
      </h2>
      {unknown && <UnknownBadge />}
      <span className="min-w-0 flex-1 text-[13px] desk:text-[13.5px] leading-[1.4]">
        {desktop
          ? t("failed.single", {
              title: item.title,
              reason: t(`failure.${failureCode(item)}`),
            })
          : t("failed.singleShort", { title: item.title })}
        {nothingSent &&
          (desktop ? (
            <>
              {" "}
              <b>{t("failed.nothingSent")}</b>
            </>
          ) : (
            <> · {t("failed.nothingSentShort")}</>
          ))}
      </span>
      <Link
        to="/work/$itemId"
        params={{ itemId: item.id }}
        search={(previous) => previous}
        className="stretch-link row ml-auto text-[13.5px] font-medium text-[var(--blue)] no-underline whitespace-nowrap"
      >
        {desktop ? (
          t("failed.review")
        ) : (
          <>
            <span className="sr-only">{t("failed.review")}</span>
            <ChevronRight />
          </>
        )}
      </Link>
    </section>
  );
}

function UnknownBadge() {
  const { t } = useTranslation("work");
  return (
    <span className="badge bg-[var(--warn2)] text-[var(--warn-ink)]">
      {t("failed.outcomeUnknown")}
    </span>
  );
}

function AllThisMonth({ data }: { data: Failures }) {
  const { t } = useTranslation("work");
  const workflows = new Set(data.items.map((item) => item.workflowKey));
  const className =
    "text-[13.5px] font-medium text-[var(--crit-ink)] underline whitespace-nowrap";
  return workflows.size === 1 ? (
    <Link
      to="/team/$key"
      params={{ key: [...workflows][0] }}
      search={{ tab: "failed" }}
      className={className}
    >
      {t("failed.allThisMonth", { count: data.month })}
    </Link>
  ) : (
    <Link to="/team" className={className}>
      {t("failed.allThisMonth", { count: data.month })}
    </Link>
  );
}

function FailureRows({ data }: { data: Failures }) {
  const { t } = useTranslation("work");
  return (
    <ul className="list divide">
      {groupFailures(data.items).map(({ code, items: grouped }) => {
        const anyUnknown = grouped.some((item) => data.unknown.has(item.id));
        const nothingSent = grouped.every((item) => !data.acted.has(item.id));
        return (
          <li key={code} className="failed-row">
            <WarnIcon />
            <span className="failed-reason">
              <b className="text-[14px] font-semibold">
                {t(`failure.${code}`)}
              </b>
              {anyUnknown && <UnknownBadge />}
            </span>
            <span className="failed-detail muted text-[13px] min-w-0 flex-1">
              {t("failed.count", { count: grouped.length })} ·{" "}
              {grouped.map((item, index) => (
                <span key={item.id}>
                  {index > 0 && ", "}
                  <Link
                    to="/work/$itemId"
                    params={{ itemId: item.id }}
                    search={(previous) => previous}
                    className="text-[var(--ink2)] underline"
                  >
                    {item.title}
                  </Link>
                </span>
              ))}
              {nothingSent && <> · {t("failed.nothingSentRow")}</>}
            </span>
            <Link
              to="/work/$itemId"
              params={{ itemId: grouped[0].id }}
              search={(previous) => previous}
              className="failed-action text-[13.5px] font-medium text-[var(--blue)] underline whitespace-nowrap"
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
  );
}

function FailureList({ data }: { data: Failures }) {
  const { t } = useTranslation("work");
  return (
    <section
      className="card p-0 overflow-hidden"
      aria-labelledby="failed-title"
    >
      <div className="row flex-wrap gap-3 px-[18px] py-3 bg-[var(--crit2)]">
        <h2
          id="failed-title"
          className="badge m-0 bg-[var(--crit2)] text-[var(--crit-ink)]"
        >
          {t("failed.title", { count: data.items.length })}
        </h2>
        <span className="text-[13.5px] text-[var(--crit-ink)] min-w-0 flex-1">
          {t("failed.lead")}
        </span>
        <AllThisMonth data={data} />
      </div>
      <FailureRows data={data} />
    </section>
  );
}

/** Phone: a summary card that opens into the same rows (XTPhoneWorkFailed). */
function PhoneFailures({ data }: { data: Failures }) {
  const { t } = useTranslation("work");
  const [open, setOpen] = useState(false);
  const summary = groupFailures(data.items)
    .map(({ code, items: grouped }) =>
      t("failed.times", {
        times: grouped.length,
        reason: t(`failure.${code}`),
      }),
    )
    .join(" · ");
  return (
    <section aria-labelledby="failed-title" className="flex flex-col gap-2">
      <button
        type="button"
        className="card failed-phone"
        aria-expanded={open}
        onClick={() => setOpen((value) => !value)}
      >
        <span className="row gap-2">
          <span
            id="failed-title"
            className="badge bg-[var(--crit2)] text-[var(--crit-ink)]"
          >
            {t("failed.title", { count: data.items.length })}
          </span>
          <span className="text-[12.5px] text-[var(--crit-ink)]">
            {t("failed.lastWeek")}
          </span>
          <span className={open ? "rotate-90" : ""}>
            <ChevronRight stroke="var(--crit-ink)" />
          </span>
        </span>
        <span className="text-[13px] leading-[1.4] text-[var(--crit-ink)] text-left">
          {summary}
          {data.unknown.size > 0 && <> ({t("failed.unknownShort")})</>}.{" "}
          {t("failed.leadShort")}
        </span>
      </button>
      {open && (
        <div className="card p-0 overflow-hidden">
          <FailureRows data={data} />
          <div className="px-[14px] py-3 border-t border-[var(--line2)]">
            <AllThisMonth data={data} />
          </div>
        </div>
      )}
    </section>
  );
}
