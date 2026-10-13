import type { LocalizedText, PauseReason, WorkState } from "../types";
import { fixedT, type Locale } from "./index";

// Helpers for the pre-V2 API-key dashboard (legacy namespace). HOR-459 deletes
// them with LegacyDashboard.

export const legacyT = (locale: Locale) => fixedT(locale, "legacy");

/** Picks the server-provided customer copy for the active locale. */
export function localized(
  value: LocalizedText | undefined,
  locale: Locale,
): string {
  if (!value) return "";
  return value[locale] || value.en || value.pt || "";
}
export function stateLabel(state: WorkState, locale: Locale): string {
  const t = legacyT(locale);
  return state === "failed" ? t("failedState") : t(state);
}
export function pauseReasonLabel(reason: PauseReason, locale: Locale): string {
  return legacyT(locale)(reason);
}
export function waitingProgress(
  received: number,
  expected: number,
  locale: Locale,
): string {
  return legacyT(locale)("waitingProgress", { received, expected });
}
