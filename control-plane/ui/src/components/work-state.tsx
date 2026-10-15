import { useTranslation } from "react-i18next";
import type { WorkItem } from "../lib/types";

// A work item's state as the boards' badge: a small dot and the words. The
// words carry the state; colour only repeats it. Self-contained (.state-badge
// in work.css) so it renders the same inside portals outside the .xt root.

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
    <span className={`state-badge state-badge--${item.state}`}>
      <span className="state-badge__dot" aria-hidden="true" />
      {label}
    </span>
  );
}
