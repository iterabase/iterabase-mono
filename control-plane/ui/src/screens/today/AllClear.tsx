import { Link, useRouterState } from "@tanstack/react-router";
import { Trans, useTranslation } from "react-i18next";
import { useLive, useLiveQuery } from "../../app/live";
import { useSession } from "../../app/session";
import { CoworkerMark } from "../../components/coworker";
import { useLocale } from "../../i18n";
import { formatTime } from "../../i18n/format";
import { listWorkItems } from "../../lib/api";
import { firstName, nextDeadline } from "../../work/model";
import { DoneCheck, LockIcon } from "./icons";
import { SettledPile, useIsDesk } from "./parts";

// All clear (XTAllClear, XTPhoneAllClear): the end of Your turn. It shows in
// the app frame with Today current, as the boards draw it, so Your turn hands
// what was cleared to Today through the history entry's state.

/** One decision cleared in this Your turn session. */
export interface ClearedDecision {
  id: string;
  /** The question or approval as the coworker asked it. */
  title: string;
  /** Where the work came from (customer, mailbox, schedule). */
  source: string;
  /** The declared outcome label the person chose. */
  outcome: string;
  at: string;
}

export interface AllClearState {
  cleared: ClearedDecision[];
  skipped: number;
  seconds: number;
}

declare module "@tanstack/history" {
  interface HistoryState {
    allClear?: AllClearState;
  }
}

export function useAllClear(): AllClearState | undefined {
  return useRouterState({
    select: (state) => state.location.state.allClear,
  });
}

export function AllClear({ state }: { state: AllClearState }) {
  const { t } = useTranslation("today");
  const locale = useLocale();
  const desk = useIsDesk();
  const session = useSession();
  const { workflows } = useLive();
  const waiting = useLiveQuery(() => listWorkItems({ state: "waiting" }), []);
  const next =
    waiting.status === "ready" ? nextDeadline(waiting.data) : undefined;
  const me = firstName(session.profile.displayName, session.profile.email);
  const { cleared, skipped, seconds } = state;
  const time = {
    count: cleared.length,
    minutes: Math.floor(seconds / 60),
    seconds: seconds % 60,
  };

  const lead =
    skipped > 0 ? (
      t("allClear.skipped", { count: skipped })
    ) : next ? (
      <Trans
        t={t}
        i18nKey={desk ? "allClear.next" : "allClear.nextShort"}
        values={{ at: formatTime(next, locale) }}
        components={{ b: <b className="text-[var(--ink)]" /> }}
      />
    ) : cleared.length === 0 ? (
      t("allClear.nothingToClear")
    ) : (
      t("allClear.nothingNext")
    );

  return (
    <div className="allclear">
      <div className="glow allclear-glow" aria-hidden="true" />
      <SettledPile small={!desk} />
      <h1
        className={
          desk
            ? "serif relative mt-[14px] mb-0 text-[44px] leading-[1.08]"
            : "serif relative mt-2 mb-0 text-[28px] leading-[1.12]"
        }
      >
        {t("allClear.title")}
      </h1>
      <p
        className={
          desk
            ? "relative m-0 max-w-[560px] text-[18px] leading-[1.5] text-[var(--ink2)]"
            : "relative m-0 text-[15px] leading-[1.5] text-[var(--ink2)]"
        }
      >
        {lead}
      </p>
      {cleared.length > 0 &&
        (desk ? (
          <ul className="list card divide relative w-[560px] max-w-full px-[18px] py-[6px] mt-[10px]">
            {cleared.map((entry) => (
              <li key={entry.id} className="row gap-[14px] py-[11px] px-[2px]">
                <span className="mono muted text-[12.5px] w-[40px] flex-none text-left">
                  {formatTime(entry.at, locale)}
                </span>
                <DoneCheck size={24} />
                <span className="text-[14px] leading-[1.35] text-left min-w-0">
                  <b className="font-semibold">{entry.title}</b>
                  <br />
                  <span className="muted text-[12.5px]">
                    {entry.source} · {entry.outcome}
                  </span>
                </span>
                <span className="ml-auto flex-none text-[12.5px] text-[var(--ok-ink)]">
                  {t("allClear.by", { name: me })}
                </span>
              </li>
            ))}
          </ul>
        ) : (
          <ul className="list card divide relative w-full p-0 mt-[6px]">
            {cleared.map((entry) => (
              <li key={entry.id} className="row gap-3 px-[14px] py-[11px]">
                <DoneCheck size={22} />
                <span className="text-[13.5px] leading-[1.35] text-left min-w-0">
                  <b className="font-semibold">{entry.title}</b>
                  <br />
                  <span className="muted text-[12px]">
                    {entry.source} · {entry.outcome} ·{" "}
                    {t("allClear.byLower", { name: me })}
                  </span>
                </span>
              </li>
            ))}
          </ul>
        ))}
      {cleared.length > 0 && (
        <span
          className={`mono muted relative ${desk ? "text-[12.5px]" : "text-[12px]"}`}
        >
          {t(desk ? "allClear.summary" : "allClear.summaryShort", time)}
        </span>
      )}
      {desk ? (
        <div className="row relative gap-[10px] mt-[6px]">
          <Link to="/" className="btn btn-ink">
            {t("allClear.backToToday")}
          </Link>
          <Link
            to="/work"
            search={{ period: "today" }}
            className="btn btn-secondary"
          >
            {t("allClear.seeAllWork")}
          </Link>
        </div>
      ) : (
        <Link to="/" className="btn btn-ink btn-full relative">
          {t("allClear.backToToday")}
        </Link>
      )}
      {desk && workflows && workflows.length > 0 && (
        <div className="row relative gap-[10px] mt-[18px]" aria-hidden="true">
          {workflows.map((workflow) => (
            <CoworkerMark
              key={workflow.key}
              coworker={workflow.coworker}
              size={32}
              state="done"
              decorative
            />
          ))}
        </div>
      )}
      {desk && (
        <footer className="allclear-trust">
          <span className="row gap-[7px] text-[12.5px] text-[var(--ink3)]">
            <LockIcon />
            <span>{t("trustLine", { ns: "app" })}</span>
          </span>
        </footer>
      )}
    </div>
  );
}
