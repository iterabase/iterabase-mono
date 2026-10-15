import { useTranslation } from "react-i18next";
import type { Coworker, CoworkerMark as Mark } from "../lib/types";
import type { CoworkerState } from "../work/model";
import { localized } from "../work/model";
import { cn } from "../lib/cn";
import { useLocale } from "../i18n";
import type { ReactNode } from "react";

// AI coworker character marks (DS README §6, components/Coworkers). Shape and
// colour come from the workflow's declared mark (DES-HOR-459-01); the face
// shows the live state: a flat mouth when failed, smiling eyes when done, a
// slow breathe while working.

const shapes: Record<Mark["shape"], string> = {
  round: "M50 6c22 0 40 16 40 40 0 26-17 46-40 46S10 72 10 46C10 22 28 6 50 6z",
  square:
    "M36 10h28a26 26 0 0 1 26 26v28a26 26 0 0 1-26 26H36a26 26 0 0 1-26-26V36a26 26 0 0 1 26-26z",
  petal:
    "M50 8c10 0 16 8 20 14 8 2 20 8 20 24 0 12-6 20-14 24-4 12-14 20-26 20S28 82 24 70c-8-4-14-12-14-24 0-16 12-22 20-24 4-6 10-14 20-14z",
};

export type MarkState = CoworkerState | "done";

export function CoworkerMark({
  coworker,
  size = 36,
  state,
  decorative = false,
}: {
  coworker: Pick<Coworker, "name" | "mark">;
  size?: number;
  state?: MarkState;
  decorative?: boolean;
}) {
  const { t } = useTranslation("app");
  const label = t("coworkerLabel", { name: coworker.name });
  return (
    <svg
      className={cn(
        "ib-coworker",
        state === "working" && "ib-coworker--working",
      )}
      width={size}
      height={size}
      viewBox="0 0 100 100"
      {...(decorative
        ? { "aria-hidden": true, focusable: false }
        : { role: "img", "aria-label": label })}
    >
      <path
        d={shapes[coworker.mark.shape] ?? shapes.round}
        className={`cw-${coworker.mark.color}`}
      />
      {state === "done" ? (
        <g stroke="#FFFFFF" strokeWidth="4.2" fill="none" strokeLinecap="round">
          <path d="M33 48q5-6 10 0" />
          <path d="M57 48q5-6 10 0" />
        </g>
      ) : (
        <g fill="#FFFFFF">
          <ellipse cx="38" cy="47" rx="4.6" ry="5.6" />
          <ellipse cx="62" cy="47" rx="4.6" ry="5.6" />
        </g>
      )}
      {state === "failed" ? (
        <path
          d="M41 65h18"
          stroke="#FFFFFF"
          strokeWidth="4.2"
          fill="none"
          strokeLinecap="round"
        />
      ) : state === "done" ? (
        <path
          d="M40 61q10 9 20 0"
          stroke="#FFFFFF"
          strokeWidth="4.2"
          fill="none"
          strokeLinecap="round"
        />
      ) : (
        <path
          d="M41 62q9 6 18 0"
          stroke="#FFFFFF"
          strokeWidth="4.2"
          fill="none"
          strokeLinecap="round"
        />
      )}
    </svg>
  );
}

/** A live coworker status: dot plus a word, never colour alone. */
export function CoworkerStatus({ state }: { state: CoworkerState }) {
  const { t } = useTranslation("app");
  return (
    <span className={`ib-status ib-status--${state}`}>
      <span className="ib-status__dot" aria-hidden="true" />
      {t(`coworkerState.${state}`)}
    </span>
  );
}

/** Mark, name, role, live status and one line of what it is doing. */
export function CoworkerLine({
  coworker,
  state,
  text,
  size = 36,
}: {
  coworker: Coworker;
  state?: CoworkerState;
  text?: ReactNode;
  size?: number;
}) {
  const locale = useLocale();
  return (
    <div className="ib-coworker-line">
      <CoworkerMark coworker={coworker} size={size} state={state} decorative />
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
          <span className="ib-coworker-line__name">{coworker.name}</span>
          <span className="ib-coworker-line__role">
            {localized(coworker.role, locale)}
          </span>
          {state && <CoworkerStatus state={state} />}
        </div>
        {text && <div className="ib-coworker-line__text">{text}</div>}
      </div>
    </div>
  );
}

const placeholderMarks: Mark[] = [
  { shape: "round", color: "blue" },
  { shape: "square", color: "green" },
  { shape: "petal", color: "sun" },
];

/**
 * A new installation has no AI coworkers yet: the three generic marks (as on
 * sign-in) with a title and a line. Compact on Today, a full card on Team.
 */
export function NoCoworkers({
  title,
  body,
  compact = false,
}: {
  title?: string;
  body: string;
  compact?: boolean;
}) {
  return (
    <div
      className={cn("card no-coworkers", compact && "no-coworkers--compact")}
    >
      <span className="no-coworkers__marks" aria-hidden="true">
        {placeholderMarks.map((mark) => (
          <CoworkerMark
            key={mark.shape}
            coworker={{ name: "", mark }}
            size={compact ? 28 : 40}
            decorative
          />
        ))}
      </span>
      <div className="flex min-w-0 flex-col gap-1">
        {title && <h2 className="no-coworkers__title">{title}</h2>}
        <p className="m-0 muted">{body}</p>
      </div>
    </div>
  );
}
