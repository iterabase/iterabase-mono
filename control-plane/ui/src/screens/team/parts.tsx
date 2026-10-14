import { Fragment, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useLocale } from "../../i18n";
import { formatNumber, formatShortDate, formatTime } from "../../i18n/format";
import type { WorkItem, WorkState } from "../../lib/types";
import { startOfDay, type CoworkerState } from "../../work/model";

// Small pieces the Team and AI coworker boards repeat: the board's line
// icons, the live dot, the work-state badge, the hours-back figure, item
// references and day groups.

type IconProps = { size?: number; className?: string };

function Line({
  size = 14,
  stroke = "currentColor",
  width = 2,
  className,
  children,
}: IconProps & {
  stroke?: string;
  width?: number;
  children: ReactNode;
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke={stroke}
      strokeWidth={width}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      className={`flex-none ${className ?? ""}`}
    >
      {children}
    </svg>
  );
}

export const CheckMark = (props: IconProps) => (
  <Line stroke="var(--ok)" width={2.6} {...props}>
    <path d="M5 12l5 5 9-10" />
  </Line>
);

export const LockMark = ({
  width = 2.6,
  stroke = "var(--sun)",
  ...props
}: IconProps & { width?: number; stroke?: string }) => (
  <Line stroke={stroke} width={width} {...props}>
    <rect x="5" y="11" width="14" height="9" rx="2" />
    <path d="M8 11V8a4 4 0 0 1 8 0v3" />
  </Line>
);

export const ChevronLeft = (props: IconProps & { width?: number }) => (
  <Line {...props}>
    <path d="M15 18l-6-6 6-6" />
  </Line>
);

export const ChevronRight = (props: IconProps) => (
  <Line stroke="var(--ink3)" {...props}>
    <path d="M9 6l6 6-6 6" />
  </Line>
);

export const RestartMark = (props: IconProps) => (
  <Line width={2.2} {...props}>
    <path d="M4 12a8 8 0 1 0 3-6.2" />
    <path d="M4 4v4h4" />
  </Line>
);

/** The flow's connector arrow between two steps. */
export const FlowArrow = () => (
  <svg
    width="26"
    height="10"
    viewBox="0 0 26 10"
    aria-hidden="true"
    focusable="false"
    className="cw-flow__arrow"
  >
    <path d="M0 5h18" stroke="#C9C2B6" strokeWidth="1.6" />
    <path d="M18 1l8 4-8 4z" fill="#C9C2B6" />
  </svg>
);

/** The dot colour the boards give each coworker state. */
const stateDot: Record<CoworkerState, string> = {
  working: "bg-[var(--ok)] live",
  available: "bg-[var(--sun)]",
  waiting: "bg-[#8C97A6]",
  failed: "bg-[var(--crit)]",
};

export function StateDot({
  state,
  className,
}: {
  state: CoworkerState;
  className?: string;
}) {
  return (
    <span
      className={`dot ${stateDot[state]} ${className ?? ""}`}
      aria-hidden="true"
    />
  );
}

/** A work item's state as the boards' badge: dot plus the word. */
const badgeTone: Record<WorkState, string> = {
  blocked: "bg-[var(--sun2)] text-[var(--sun-ink)]",
  waiting: "bg-[var(--paper2)] text-[var(--ink2)]",
  in_progress: "bg-[var(--ok2)] text-[var(--ok-ink)]",
  todo: "bg-[var(--ok2)] text-[var(--ok-ink)]",
  done: "bg-[var(--ok2)] text-[var(--ok-ink)]",
  failed: "bg-[var(--crit2)] text-[var(--crit-ink)]",
  paused: "bg-[var(--warn2)] text-[var(--warn-ink)]",
};

export function WorkBadge({
  item,
  dot = true,
}: {
  item: Pick<WorkItem, "state">;
  dot?: boolean;
}) {
  const { t } = useTranslation("work");
  return (
    <span className={`badge ${badgeTone[item.state]}`}>
      {dot && (
        <span
          className="dot size-[6px] bg-current opacity-90"
          aria-hidden="true"
        />
      )}
      {t(`state.${item.state}`)}
    </span>
  );
}

/** "≈ 24.5 h": estimated seconds back as hours, one decimal at most. */
export function useHoursBack(): (seconds: number) => string {
  const { t } = useTranslation("team");
  const locale = useLocale();
  return (seconds) =>
    t("hours", {
      hours: formatNumber(Math.round(seconds / 360) / 10, locale, {
        maximumFractionDigits: 1,
      }),
    });
}

/**
 * A short, stable reference for a work item. Platform ids are UUIDs; the
 * first block is enough to tell items apart on one screen.
 */
export function itemRef(id: string): string {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-/i.test(id)
    ? id.slice(0, 8).toUpperCase()
    : id;
}

/** When an item last moved, for the activity lists. */
export function activityAt(item: WorkItem): string {
  return item.finishedAt ?? item.createdAt;
}

/** "09:58" today, "13 Oct · 09:58" before today (or always, for failures). */
export function useWhen(): (at: string, always?: boolean) => string {
  const locale = useLocale();
  return (at, always = false) => {
    const time = formatTime(at, locale);
    return !always && new Date(at) >= startOfDay(new Date())
      ? time
      : `${formatShortDate(at, locale)} · ${time}`;
  };
}

/** Items newest first. */
export function newestFirst(items: WorkItem[]): WorkItem[] {
  return [...items].sort(
    (a, b) =>
      new Date(activityAt(b)).getTime() - new Date(activityAt(a)).getTime(),
  );
}

/** Items newest first, grouped by the day they last moved. */
export function byDay(
  items: WorkItem[],
): Array<{ day: Date; items: WorkItem[] }> {
  const groups = new Map<number, WorkItem[]>();
  for (const item of newestFirst(items)) {
    const day = startOfDay(new Date(activityAt(item))).getTime();
    groups.set(day, [...(groups.get(day) ?? []), item]);
  }
  return [...groups.entries()].map(([day, grouped]) => ({
    day: new Date(day),
    items: grouped,
  }));
}

/** "Today · 13 Oct" or "12 Oct" for a day group's eyebrow. */
export function useDayLabel(): (day: Date) => string {
  const { t } = useTranslation("team");
  const locale = useLocale();
  return (day) => {
    const date = formatShortDate(day, locale);
    return day.getTime() === startOfDay(new Date()).getTime()
      ? t("activity.today", { date })
      : date;
  };
}

/** Declared copy with any email address in the mono face, as the boards set it. */
export function WithEmails({ text }: { text: string }) {
  const parts = text.split(/([\w.+-]+@[\w-]+(?:\.[\w-]+)+)/);
  return (
    <>
      {parts.map((part, index) =>
        index % 2 === 1 ? (
          <span key={index} className="mono text-[0.93em]">
            {part}
          </span>
        ) : (
          <Fragment key={index}>{part}</Fragment>
        ),
      )}
    </>
  );
}

/** Splits declared "Title · detail" copy into its title and detail. */
export function splitLine(text: string): { title: string; detail: string } {
  const [title, ...rest] = text.split(" · ");
  return { title: title ?? text, detail: rest.join(" · ") };
}
