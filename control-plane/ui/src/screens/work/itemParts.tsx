import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useSession } from "../../app/session";
import { useLocale } from "../../i18n";
import { formatTime } from "../../i18n/format";
import { artifactName, openArtifact } from "../../lib/api";
import type {
  DetailData,
  TimelineEvent,
  WorkArtifact,
  WorkState,
} from "../../lib/types";
import { localized } from "../../work/model";
import { sourceKey } from "../workLines";

// The work item's Timeline and Sources and documents (XTItem, XTPhoneItem).

const eventCodes = [
  "work_item_created",
  "attempt_created",
  "node_started",
  "node_completed",
  "work_blocked",
  "blocker_resolved",
  "work_completed",
  "work_failed",
  "feedback_recorded",
  "revision_requested",
  "consequence_confirmation_required",
  "consequence_repetition_confirmed",
  "work_waiting",
  "wait_event_received",
  "wait_stopped",
  "work_paused",
  "work_resumed",
  "step_interrupted",
] as const;

/** The event that says where the item stands is coloured: asked, failed, done. */
const standing: Partial<Record<WorkState, [string[], string]>> = {
  blocked: [["work_blocked"], "timeline__row--sun"],
  failed: [["work_failed", "step_interrupted"], "timeline__row--crit"],
  done: [["work_completed"], "timeline__row--ok"],
};

export function Timeline({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const nodes = new Map(data.nodes.map((node) => [node.id, node]));
  const events = data.timeline.filter(
    (event) => !event.code.startsWith("value_"),
  );
  // While it works, the step it is on closes the timeline as a live row.
  const working =
    data.item.state === "in_progress" || data.item.state === "todo";
  const current = working ? data.item.currentStep : undefined;
  const label = (event: TimelineEvent) => {
    const code = eventCodes.find((known) => known === event.code);
    return code ? t(`event.${code}`) : t("event.work_updated");
  };
  return (
    <section className="flex flex-col gap-1 desk:gap-[6px]">
      <h3 className="eyebrow m-0">{t("item.timeline")}</h3>
      <ol className="list">
        {events.map((event, index) => {
          const node = event.nodeExecutionId
            ? nodes.get(event.nodeExecutionId)
            : undefined;
          const [codes, toneClass] = standing[data.item.state] ?? [[], ""];
          const tone =
            index === events.length - 1 && codes.includes(event.code)
              ? toneClass
              : "";
          return (
            <li key={event.id} className={`timeline__row ${tone}`}>
              <time
                className="mono muted timeline__time"
                dateTime={event.createdAt}
              >
                {formatTime(event.createdAt, locale)}
              </time>
              <span className="dot timeline__dot" aria-hidden="true" />
              <span className="leading-[1.4] min-w-0">
                {label(event)}
                {node && <> · {localized(node.businessLabel, locale)}</>}
                {event.actorDisplayName && <> · {event.actorDisplayName}</>}
              </span>
            </li>
          );
        })}
        {current && (
          <li className="timeline__row timeline__row--ok">
            <span className="mono timeline__time">{t("item.nowTitle")}</span>
            <span className="dot live timeline__dot" aria-hidden="true" />
            <span className="leading-[1.4] min-w-0">
              {localized(current.label, locale)}
            </span>
          </li>
        )}
      </ol>
    </section>
  );
}

function artifactKind(
  artifact: WorkArtifact,
): "email" | "pdf" | "image" | "file" {
  if (artifact.mimeType === "message/rfc822") return "email";
  if (artifact.mimeType === "application/pdf") return "pdf";
  if (artifact.mimeType.startsWith("image/")) return "image";
  return "file";
}

const emailKinds = ["email", "graph_email", "outlook"];

export function Sources({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const session = useSession();
  const [failed, setFailed] = useState(false);
  const { source } = data.item;
  const isEmail = emailKinds.includes(source.kind);
  return (
    <section className="flex flex-col gap-[6px] desk:gap-[2px]">
      <h3 className="eyebrow m-0">{t("item.sources")}</h3>
      <ul className="list divide sources">
        <li className="source-row">
          <span className="badge source-row__pill">
            {t(`source.${sourceKey(source.kind)}`)}
          </span>
          <span className="source-row__name">
            {source.subtitle || source.title}
          </span>
          {source.originalUrl && (
            <a
              href={source.originalUrl}
              target="_blank"
              rel="noreferrer noopener"
              className="source-row__view"
            >
              {isEmail ? t("item.viewEmail") : t("item.openOriginal")}
            </a>
          )}
          {!!source.evidence?.length && (
            <dl className="source-row__evidence">
              {source.evidence.map((field, index) => (
                <div key={index} className="contents">
                  <dt className="muted">{localized(field.label, locale)}</dt>
                  <dd className="m-0 min-w-0 break-words">{field.value}</dd>
                </div>
              ))}
            </dl>
          )}
        </li>
        {data.artifacts.map((artifact) => {
          const kind = artifactKind(artifact);
          return (
            <li
              key={`${artifact.artifactId}-${artifact.role}`}
              className="source-row"
            >
              <span className="badge source-row__pill">
                {t(`artifact.${kind}`)}
              </span>
              <span className="source-row__name">{artifactName(artifact)}</span>
              <button
                type="button"
                className="source-row__view"
                onClick={() =>
                  void openArtifact(artifact).catch((err: unknown) => {
                    if (!session.lost(err)) setFailed(true);
                  })
                }
              >
                {t(`artifact.view.${kind}`)}
                <span className="sr-only"> {artifactName(artifact)}</span>
              </button>
            </li>
          );
        })}
      </ul>
      {failed && (
        <p role="alert" className="form-error">
          {t("item.artifactFailed")}
        </p>
      )}
    </section>
  );
}
