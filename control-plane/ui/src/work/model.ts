// Pure projections of the work API for the app's screens. Everything shown is
// derived from server state; nothing here predicts or invents progress.
import type { Locale } from "../i18n";
import type {
  LocalizedText,
  WorkItem,
  WorkState,
  WorkflowSummary,
} from "../lib/types";

/** The four Work groups (plus Failed, which has its own strip). */
export type Group = "working" | "waiting" | "needs" | "done" | "failed";
export const boardGroups: Exclude<Group, "failed">[] = [
  "working",
  "waiting",
  "needs",
  "done",
];

/**
 * Waiting and paused work never needs the customer (DES-HOR-649-05): it sits
 * in Waiting. Needs you holds only work blocked on a person.
 */
export function groupOf(state: WorkState): Group {
  switch (state) {
    case "todo":
    case "in_progress":
      return "working";
    case "waiting":
    case "paused":
      return "waiting";
    case "blocked":
      return "needs";
    case "done":
      return "done";
    case "failed":
      return "failed";
  }
}

export function groupItems(items: WorkItem[]): Record<Group, WorkItem[]> {
  const groups: Record<Group, WorkItem[]> = {
    working: [],
    waiting: [],
    needs: [],
    done: [],
    failed: [],
  };
  for (const item of items) groups[groupOf(item.state)].push(item);
  groups.needs.sort(oldestFirst);
  groups.done.sort((a, b) => time(b.finishedAt) - time(a.finishedAt));
  return groups;
}

function time(value?: string): number {
  return value ? new Date(value).getTime() : 0;
}

export function oldestFirst(a: WorkItem, b: WorkItem): number {
  return time(a.updatedAt) - time(b.updatedAt);
}

/** Everything waiting on a person, oldest first (REQ-052). */
export function decisionsOf(items: WorkItem[]): WorkItem[] {
  return items.filter((item) => item.state === "blocked").sort(oldestFirst);
}

export type PeriodPreset = "today" | "week" | "month";

export function startOfDay(date: Date): Date {
  const d = new Date(date);
  d.setHours(0, 0, 0, 0);
  return d;
}

export function periodFor(
  preset: PeriodPreset,
  now = new Date(),
): { from: Date; to: Date } {
  const from = startOfDay(now);
  const to = new Date(from);
  to.setDate(to.getDate() + 1);
  if (preset === "week")
    from.setDate(from.getDate() - ((from.getDay() + 6) % 7));
  if (preset === "month") from.setDate(1);
  return { from, to };
}

export function daysAgo(days: number, now = new Date()): Date {
  const d = startOfDay(now);
  d.setDate(d.getDate() - days);
  return d;
}

/** Failure reasons the platform records (internal/work/stop.go, dispatch). */
export const knownFailureCodes = [
  "execution_failed",
  "transition_limit",
  "invalid_wait_input",
  "interrupted_after_write",
  "repeated_interruption",
] as const;
export type FailureCode = (typeof knownFailureCodes)[number] | "other";

export function failureCode(item: WorkItem): FailureCode {
  const code = item.failureSummary?.code;
  return knownFailureCodes.find((known) => known === code) ?? "other";
}

/** Failed items grouped by reason, largest group first. */
export function groupFailures(
  items: WorkItem[],
): Array<{ code: FailureCode; items: WorkItem[] }> {
  const groups = new Map<FailureCode, WorkItem[]>();
  for (const item of items) {
    const code = failureCode(item);
    groups.set(code, [...(groups.get(code) ?? []), item]);
  }
  return [...groups.entries()]
    .map(([code, grouped]) => ({ code, items: grouped }))
    .sort((a, b) => b.items.length - a.items.length);
}

/** The DS coworker states (README §6): working, available, waiting, failed. */
export type CoworkerState = "working" | "available" | "waiting" | "failed";

export function coworkerState(
  items: WorkItem[],
  now = new Date(),
): CoworkerState {
  if (items.some((item) => groupOf(item.state) === "working")) return "working";
  const dayAgo = now.getTime() - 24 * 3600 * 1000;
  if (
    items.some(
      (item) => item.state === "failed" && time(item.finishedAt) >= dayAgo,
    )
  )
    return "failed";
  if (items.some((item) => groupOf(item.state) === "waiting")) return "waiting";
  return "available";
}

/** The item a coworker's live line describes: newest active, else newest. */
export function headlineItem(items: WorkItem[]): WorkItem | undefined {
  const byRecent = [...items].sort(
    (a, b) => time(b.updatedAt) - time(a.updatedAt),
  );
  return (
    byRecent.find((item) => groupOf(item.state) === "working") ?? byRecent[0]
  );
}

/** Groups items by workflow, keeping every known coworker (even idle ones). */
export function itemsByWorkflow(
  workflows: WorkflowSummary[],
  items: WorkItem[],
): Map<string, WorkItem[]> {
  const map = new Map<string, WorkItem[]>(
    workflows.map((workflow) => [workflow.key, []]),
  );
  for (const item of items) {
    map.set(item.workflowKey, [...(map.get(item.workflowKey) ?? []), item]);
  }
  return map;
}

/** The earliest wait deadline still ahead, for "around HH:MM" (All clear). */
export function nextDeadline(
  items: WorkItem[],
  now = new Date(),
): Date | undefined {
  const deadlines = items
    .filter((item) => item.state === "waiting" && item.wait?.deadlineAt)
    .map((item) => new Date(item.wait!.deadlineAt))
    .filter((date) => date.getTime() > now.getTime())
    .sort((a, b) => a.getTime() - b.getTime());
  return deadlines[0];
}

export type Greeting = "morning" | "afternoon" | "evening";
export function greetingFor(now: Date): Greeting {
  const hour = now.getHours();
  if (hour < 12) return "morning";
  if (hour < 18) return "afternoon";
  return "evening";
}

export function firstName(displayName: string, email: string): string {
  const name = displayName.trim().split(/\s+/)[0];
  return name || email.split("@")[0] || "";
}

export function initials(name: string): string {
  return (
    name
      .split(/\s+/)
      .filter(Boolean)
      .slice(0, 2)
      .map((part) => part[0]?.toUpperCase())
      .join("") || "·"
  );
}

/** Customer copy for the active locale, falling back to the other language. */
export function localized(
  value: LocalizedText | undefined,
  locale: Locale,
): string {
  if (!value) return "";
  return value[locale] || value.en || value.pt || "";
}

/** Whole minutes between two instants, never negative. */
export function minutesBetween(from: string | Date, now = new Date()): number {
  return Math.max(
    0,
    Math.round((now.getTime() - new Date(from).getTime()) / 60_000),
  );
}

/** Rounds estimated seconds to hours and minutes for "1 h 50". */
export function hoursAndMinutes(seconds: number): {
  hours: number;
  minutes: number;
} {
  const total = Math.max(0, Math.round(seconds / 60));
  return { hours: Math.floor(total / 60), minutes: total % 60 };
}
