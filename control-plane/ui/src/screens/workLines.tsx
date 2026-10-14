import { useTranslation } from "react-i18next";
import { useLocale } from "../i18n";
import { formatShortDate, formatTime } from "../i18n/format";
import type { WorkItem } from "../lib/types";
import {
  failureCode,
  localized,
  minutesBetween,
  startOfDay,
} from "../work/model";

// One honest line about where a work item is, from its projection: the
// question it asks, what it waits for, why it is paused, the step it is on,
// or why it failed.

export function StatusLine({ item }: { item: WorkItem }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  switch (item.state) {
    case "blocked":
      return (
        <>{localized(item.blocker?.title, locale) || t("line.needsYou")}</>
      );
    case "waiting":
      return item.wait ? (
        <>
          {t("line.waiting", {
            received: item.wait.received,
            expected: item.wait.expected,
            at: formatTime(item.wait.deadlineAt, locale),
          })}
        </>
      ) : (
        <>{t("line.waitingPlain")}</>
      );
    case "paused":
      return <>{t(`pause.${item.pause?.reason ?? "other"}`)}</>;
    case "failed":
      return <>{t(`failure.${failureCode(item)}`)}</>;
    case "done":
      return (
        <>{localized(item.currentStep?.label, locale) || t("line.done")}</>
      );
    default:
      return (
        <>{localized(item.currentStep?.label, locale) || t("line.starting")}</>
      );
  }
}

const sourceKinds = [
  "email",
  "graph_email",
  "outlook",
  "api",
  "schedule",
  "scheduled",
  "chat",
  "artifact",
] as const;

/** "Email · 22 min" / "API · 09:41" / "Email · 12 Oct". */
export function SourceLine({ item }: { item: WorkItem }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const kind = sourceKinds.find((known) => known === item.source.kind);
  const at = item.createdAt;
  const minutes = minutesBetween(at);
  const when =
    minutes < 1
      ? t("now")
      : minutes < 60
        ? t("minutesShort", { count: minutes })
        : new Date(at) >= startOfDay(new Date())
          ? formatTime(at, locale)
          : formatShortDate(at, locale);
  return (
    <>
      {kind ? t(`source.${kind}`) : t("source.other")} · {when}
    </>
  );
}
