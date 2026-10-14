import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useLocale } from "../../i18n";
import { formatNumber } from "../../i18n/format";
import type { WorkflowSummary } from "../../lib/types";
import { localized } from "../../work/model";
import { CheckMark, LockMark, splitLine } from "./parts";

// What it can do (boards XTCoworkerPermissions / XTPhoneCoworkerPermissions):
// the declared on-its-own lines, the always-asks lines derived from the human
// gates, and the connections it uses. The API classifies none of the
// on-its-own lines as reading or sending, so they carry no pill; an ask's
// pill follows its gate type (approval: you confirm, information: you answer).

export function WhatItCanDo({ workflow }: { workflow: WorkflowSummary }) {
  const { t } = useTranslation("team");
  const locale = useLocale();
  const onItsOwn = workflow.coworker.onItsOwn ?? [];
  const connections = workflow.coworker.connections ?? [];
  return (
    <div className="cw-can">
      <Panel
        id="cw-own"
        title={t("can.onItsOwn")}
        count={formatNumber(onItsOwn.length, locale)}
      >
        {onItsOwn.length === 0 ? (
          <p className="cw-panel__empty">{t("nothingOnItsOwn")}</p>
        ) : (
          <ul className="cw-panel__list">
            {onItsOwn.map((line, index) => (
              <li key={index}>
                <CheckMark size={16} className="cw-panel__icon" />
                <b className="cw-panel__title">{localized(line, locale)}</b>
              </li>
            ))}
          </ul>
        )}
      </Panel>

      <Panel
        id="cw-asks"
        title={t("can.alwaysAsksFirst")}
        count={formatNumber(workflow.alwaysAsks.length, locale)}
      >
        {workflow.alwaysAsks.length === 0 ? (
          <p className="cw-panel__empty">{t("asksNothing")}</p>
        ) : (
          <ul className="cw-panel__list">
            {workflow.alwaysAsks.map((ask) => (
              <li key={ask.nodeKey}>
                <LockMark size={16} className="cw-panel__icon" />
                <b className="cw-panel__title">
                  {localized(ask.title, locale)}
                </b>
                <span className="ml-auto hidden flex-none desk:inline-flex">
                  <span className="badge bg-[var(--sun2)] text-[var(--sun-ink)]">
                    {ask.type === "approval" ? t("youConfirm") : t("youAnswer")}
                  </span>
                </span>
              </li>
            ))}
          </ul>
        )}
        <p className="cw-panel__note">{t("can.anythingElse")}</p>
      </Panel>

      <Panel id="cw-conn" title={t("can.connections")}>
        {connections.length === 0 ? (
          <p className="cw-panel__empty">{t("noConnections")}</p>
        ) : (
          <ul className="cw-panel__list cw-panel__list--conn">
            {connections.map((line, index) => {
              const { title, detail } = splitLine(localized(line, locale));
              return (
                <li key={index}>
                  <span className="flex min-w-0 flex-col gap-[2px]">
                    <b className="cw-panel__title">{title}</b>
                    {detail && (
                      <span className="muted hidden text-[12.5px] desk:block">
                        {detail}
                      </span>
                    )}
                  </span>
                </li>
              );
            })}
          </ul>
        )}
      </Panel>
    </div>
  );
}

function Panel({
  id,
  title,
  count,
  children,
}: {
  id: string;
  title: string;
  count?: string;
  children: ReactNode;
}) {
  return (
    <section className="cw-panel" aria-labelledby={id}>
      <h2 id={id} className="cw-panel__head">
        <span>{title}</span>
        {count !== undefined && (
          <span className="cw-panel__count">
            <span className="desk:hidden" aria-hidden="true">
              {" · "}
            </span>
            {count}
          </span>
        )}
      </h2>
      {children}
    </section>
  );
}
