import { useTranslation } from "react-i18next";
import { CoworkerMark } from "../../components/coworker";
import { useLocale } from "../../i18n";
import type { StepKind, WorkflowSummary, WorkItem } from "../../lib/types";
import { groupOf, localized } from "../../work/model";
import { FlowArrow } from "./parts";

// How it works (boards XTCoworker / XTPhoneCoworker): the workflow's steps in
// order, each with the live count of items at that step. The definition has
// no branch conditions or shares, so every step sits on one line: the board's
// node anatomy without the "Only if …" text.

const nodeTone: Record<StepKind, string> = {
  agent_task: "cw-node--agent",
  human_gate: "cw-node--you",
  wait: "cw-node--wait",
};

export function HowItWorks({
  workflow,
  items,
  live,
}: {
  workflow: WorkflowSummary;
  items: WorkItem[];
  /** False when the counts could not be refreshed: no "Live now". */
  live: boolean;
}) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const name = workflow.coworker.name;
  const active = items.filter(
    (item) => groupOf(item.state) !== "done" && item.state !== "failed",
  );

  return (
    <>
      <section className="cw-how" aria-labelledby="cw-how-title">
        <h2 id="cw-how-title" className="sr-only">
          {t("tabs.how")}
        </h2>
        <div className="cw-how__head">
          {live && (
            <span className="flex items-center gap-[7px]">
              <span className="dot live bg-[var(--ok)]" aria-hidden="true" />
              <b className="font-semibold text-[var(--ink)]">{t("how.live")}</b>
            </span>
          )}
          <span className="ml-auto hidden items-center gap-[7px] desk:flex">
            <CoworkerMark coworker={workflow.coworker} size={14} decorative />
            {t("how.does", { name })}
          </span>
          <span className="hidden items-center gap-[7px] desk:flex">
            <span className="h-[10px] w-[14px] rounded-[3px] border border-[#EFD3B2] bg-[var(--sun2)]" />
            {t("how.youDecide")}
          </span>
          <span className="hidden items-center gap-[7px] desk:flex">
            <span className="h-[10px] w-[14px] rounded-[3px] border border-dashed border-[#B9C3CF]" />
            {t("how.waits")}
          </span>
        </div>
        <div className="cw-how__body">
          <ol className="cw-flow" aria-label={t("how.flowLabel", { name })}>
            {workflow.steps.map((step, index) => {
              const here = active.filter(
                (item) => item.currentStep?.key === step.key,
              );
              const needs = here.filter(
                (item) => groupOf(item.state) === "needs",
              ).length;
              const waiting = here.filter(
                (item) => groupOf(item.state) === "waiting",
              ).length;
              const working = here.length - needs - waiting;
              return (
                <li key={step.key}>
                  <span className="cw-flow__rail" aria-hidden="true">
                    <span />
                    <span />
                    <span />
                  </span>
                  <div className={`cw-node ${nodeTone[step.kind]}`}>
                    <span className="cw-node__kind">
                      {step.kind === "agent_task" && (
                        <CoworkerMark
                          coworker={workflow.coworker}
                          size={16}
                          decorative
                        />
                      )}
                      <span className="eyebrow">
                        {step.kind === "agent_task"
                          ? name
                          : step.kind === "human_gate"
                            ? t("how.you")
                            : t("how.waits")}
                      </span>
                    </span>
                    <b className="cw-node__label">
                      {localized(step.label, locale)}
                    </b>
                    {working > 0 && (
                      <span className="cw-node__count text-[var(--ok-ink)]">
                        <span className="dot live bg-[var(--ok)]" />
                        {t("live.working", { count: working })}
                      </span>
                    )}
                    {waiting > 0 && (
                      <span className="cw-node__count text-[var(--ink3)]">
                        <span className="dot bg-[#8C97A6]" />
                        {t("live.waiting", { count: waiting })}
                      </span>
                    )}
                    {needs > 0 && (
                      <span className="cw-node__count text-[var(--sun-ink)]">
                        <span className="dot bg-[var(--sun)]" />
                        {t("live.needs", { count: needs })}
                      </span>
                    )}
                  </div>
                  {index < workflow.steps.length - 1 && <FlowArrow />}
                </li>
              );
            })}
          </ol>
        </div>
      </section>
      <p className="cw-change">{t("how.change", { name })}</p>
    </>
  );
}
