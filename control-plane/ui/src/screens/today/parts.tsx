import { useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { CoworkerMark } from "../../components/coworker";
import { useLocale } from "../../i18n";
import type { Coworker } from "../../lib/types";
import { localized, type CoworkerState } from "../../work/model";
import { CheckIcon } from "./icons";

// Pieces the Today, Your turn and All clear boards share.

const DESK = "(min-width: 900px)";

/**
 * True at the desktop board's width (≥900px). The phone and desktop boards
 * differ in structure, not only in spacing, so the screen renders one of them.
 */
export function useIsDesk(): boolean {
  const supported =
    typeof window !== "undefined" && typeof window.matchMedia === "function";
  const [desk, setDesk] = useState(() =>
    supported ? window.matchMedia(DESK).matches : true,
  );
  useEffect(() => {
    if (!supported) return;
    const query = window.matchMedia(DESK);
    const update = () => setDesk(query.matches);
    update();
    query.addEventListener("change", update);
    return () => query.removeEventListener("change", update);
  }, [supported]);
  return desk;
}

/** A live coworker state as the boards draw it: a dot and a word. */
export function StateDot({ state }: { state: CoworkerState }) {
  const { t } = useTranslation("app");
  return (
    <>
      <span
        className={`dot state-dot--${state}${state === "working" ? " live" : ""}`}
        aria-hidden="true"
      />
      <span className={`state-word state-word--${state}`}>
        {t(`coworkerState.${state}`)}
      </span>
    </>
  );
}

/** Mark, name, role, live state and one line of what it is doing. */
export function CoworkerRow({
  coworker,
  state,
  text,
  size = 36,
}: {
  coworker: Coworker;
  state?: CoworkerState;
  text: ReactNode;
  size?: number;
}) {
  const locale = useLocale();
  return (
    <div className="row gap-3 min-w-0">
      <CoworkerMark coworker={coworker} size={size} state={state} decorative />
      <div className="flex flex-col gap-[2px] min-w-0">
        <span className="row flex-wrap gap-x-[7px]">
          <b className="text-[15px] font-semibold">{coworker.name}</b>
          <span className="muted text-[12.5px]">
            {localized(coworker.role, locale)}
          </span>
          {state && <StateDot state={state} />}
        </span>
        <span className="text-[13.5px] text-[var(--ink2)] leading-[1.4]">
          {text}
        </span>
      </div>
    </div>
  );
}

/** "● ▬ ●" progress through the decisions: done, current, still to come. */
export function ProgressDots({
  position,
  total,
}: {
  position: number;
  total: number;
}) {
  return (
    <span className="row gap-[5px]" aria-hidden="true">
      {Array.from({ length: total }, (_, index) => (
        <span
          key={index}
          className={
            index === position - 1
              ? "progress-dot progress-dot--current"
              : index < position - 1
                ? "progress-dot progress-dot--done"
                : "progress-dot"
          }
        />
      ))}
    </span>
  );
}

/** All clear's settled pile of three cards with the green check. */
export function SettledPile({ small = false }: { small?: boolean }) {
  return (
    <div className={small ? "pile pile--sm" : "pile"} aria-hidden="true">
      <div className="card pile__card pile__card--1" />
      <div className="card pile__card pile__card--2" />
      <div className="card pile__card pile__card--3" />
      <div className="pile__check">
        <CheckIcon size={small ? 20 : 26} stroke="#fff" width={3} />
      </div>
    </div>
  );
}
