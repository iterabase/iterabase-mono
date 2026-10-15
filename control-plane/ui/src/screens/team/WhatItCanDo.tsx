import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useLocale } from "../../i18n";
import { formatNumber } from "../../i18n/format";
import type { WorkflowSummary } from "../../lib/types";
import { localized } from "../../work/model";
import { CheckMark, LockMark, splitLine } from "./parts";

// What it can do (boards XTCoworkerPermissions / XTPhoneCoworkerPermissions):
// the declared on-its-own lines with their Reads/Sends/Does pill, detail and
// chips (DES-HOR-459-09), the always-asks lines derived from the human gates
// (an ask's pill follows its gate type: approval, you confirm; information,
// you answer), and the connections it uses. Connections carry no status
// until HOR-652 observes one (DES-HOR-459-11).

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
              <li key={index} className="flex-wrap">
                <CheckMark size={16} className="cw-panel__icon" />
                <span className="flex min-w-0 flex-1 flex-col gap-[2px]">
                  <b className="cw-panel__title">
                    {localized(line.text, locale)}
                  </b>
                  {line.detail && (
                    <span className="muted text-[12.5px]">
                      {localized(line.detail, locale)}
                    </span>
                  )}
                </span>
                <span className="ml-auto hidden flex-none desk:inline-flex">
                  <span className="badge bg-[var(--paper2)] text-[var(--ink2)]">
                    {t(`can.kind.${line.kind}`)}
                  </span>
                </span>
                {!!line.items?.length && (
                  <ul
                    className="cw-chips"
                    aria-label={t("can.itemsLabel", {
                      text: localized(line.text, locale),
                    })}
                  >
                    {line.items.map((value) => (
                      <li key={value} className="mono">
                        {value}
                      </li>
                    ))}
                  </ul>
                )}
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
