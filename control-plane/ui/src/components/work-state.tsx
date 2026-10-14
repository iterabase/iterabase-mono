import { useTranslation } from "react-i18next";
import type { WorkItem } from "../lib/types";

// A work item's state as a DS badge. The words carry the state; colour only
// repeats it.

const tone: Record<string, string> = {
  blocked: "ib-badge--warm",
  done: "ib-badge--ok",
  failed: "ib-badge--crit",
  paused: "ib-badge--warn",
};

export function StateBadge({
  item,
}: {
  item: Pick<WorkItem, "state" | "blocker">;
}) {
  const { t } = useTranslation("work");
  const label =
    item.state === "blocked"
      ? item.blocker?.kind === "information" ||
        item.blocker?.kind === "decision"
        ? t("state.needsAnswer")
        : t("state.needsOk")
      : t(`state.${item.state}`);
  return (
    <span className={`ib-badge ${tone[item.state] ?? "ib-badge--neutral"}`}>
      {label}
    </span>
  );
}
