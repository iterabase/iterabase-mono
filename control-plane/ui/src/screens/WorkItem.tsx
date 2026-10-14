import { Link } from "@tanstack/react-router";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useLiveQuery } from "../app/live";
import { isNotFound, useSession } from "../app/session";
import { CloseIcon, MoreIcon } from "../components/brand";
import { CoworkerMark } from "../components/coworker";
import { ErrorState, Loading, StaleNotice } from "../components/states";
import { Exact, Irreversible, Provenance } from "../components/trust";
import { Button } from "../components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogTitle,
  SheetContent,
} from "../components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "../components/ui/dropdown-menu";
import { StateBadge } from "../components/work-state";
import { useLocale } from "../i18n";
import { formatDateTime, formatTime } from "../i18n/format";
import {
  artifactName,
  createRevision,
  loadDetail,
  openArtifact,
  saveFeedback,
  stopWaiting,
} from "../lib/api";
import type { DetailData, TimelineEvent, WorkArtifact } from "../lib/types";
import { failureCode, localized } from "../work/model";
import { hasPresentedResult, PresentedResult, producingNode } from "./result";
import {
  OutcomeButtons,
  useBlockerResponse,
  type BlockerResponse,
} from "./respond";

// The work item (HOR-459 Design → Work item): status and provenance, the
// action first, the timeline, sources and documents, and the decision actions
// in a fixed footer. It opens as a side sheet over Work (full screen on
// phone); /work/<id> is also its deep link.

export function WorkItemSheet({
  itemId,
  onClose,
}: {
  itemId: string;
  onClose: () => void;
}) {
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <SheetContent aria-describedby={undefined}>
        <WorkItemView itemId={itemId} />
      </SheetContent>
    </Dialog>
  );
}

const feedbackCategories = [
  "incorrect_classification",
  "missing_information",
  "wrong_action",
  "poor_output",
] as const;

function WorkItemView({ itemId }: { itemId: string }) {
  const { t } = useTranslation("work");
  const detail = useLiveQuery(() => loadDetail(itemId), [itemId]);

  if (detail.status === "loading")
    return (
      <>
        <DialogTitle className="sr-only">{t("item.loadingTitle")}</DialogTitle>
        <Loading />
      </>
    );
  if (detail.status === "error")
    return (
      <>
        <DialogTitle className="sr-only">{t("item.errorTitle")}</DialogTitle>
        <div className="sheet__body">
          <ErrorState onRetry={detail.reload} />
        </div>
      </>
    );
  return (
    <WorkItemContent
      data={detail.data}
      stale={detail.stale}
      reload={detail.reload}
    />
  );
}

function WorkItemContent({
  data,
  stale,
  reload,
}: {
  data: DetailData;
  stale: boolean;
  reload: () => void;
}) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const [panel, setPanel] = useState<"none" | "feedback" | "restart">("none");
  const [notice, setNotice] = useState("");
  const { item } = data;
  const response = useBlockerResponse(
    item.state === "blocked" ? data.blocker : null,
    () => {
      setNotice(t("item.responseSent"));
      reload();
    },
  );

  return (
    <>
      <header className="sheet__head">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2 mb-2">
            <StateBadge item={item} />
          </div>
          <DialogTitle className="m-0 text-[22px] leading-tight font-semibold">
            {item.title}
          </DialogTitle>
          <p className="m-0 mt-1 text-sm muted">
            <Link
              to="/team/$key"
              params={{ key: item.workflowKey }}
              className="text-link"
            >
              {localized(item.presentation.title, locale)}
            </Link>{" "}
            · {t("item.byCoworker", { name: item.presentation.coworker.name })}{" "}
            · {t("item.from", { source: item.source.title })}
          </p>
        </div>
        <DropdownMenu>
          <DropdownMenuTrigger
            className="ib-iconbtn ib-iconbtn--md"
            aria-label={t("item.moreActions")}
          >
            <MoreIcon />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={() => setPanel("feedback")}>
              {t("item.giveFeedback")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
        <DialogClose
          className="ib-iconbtn ib-iconbtn--md"
          aria-label={t("item.close")}
        >
          <CloseIcon />
        </DialogClose>
      </header>

      <div className="sheet__body">
        {stale && <StaleNotice onRetry={reload} />}
        {notice && (
          <p role="status" className="auth-notice">
            {notice}
          </p>
        )}
        <ItemBody data={data} response={response} />
        {panel === "feedback" && (
          <FeedbackPanel
            data={data}
            onDone={(message) => {
              setPanel("none");
              setNotice(message);
              reload();
            }}
            onCancel={() => setPanel("none")}
          />
        )}
      </div>

      <ItemFooter
        data={data}
        response={response}
        onFeedback={() => setPanel("feedback")}
        onThanks={() => setNotice(t("item.thanks"))}
        onRestart={() => setPanel("restart")}
      />
      {panel === "restart" && (
        <RestartDialog
          data={data}
          onClose={() => setPanel("none")}
          onStarted={() => {
            setPanel("none");
            setNotice(t("item.restartStarted"));
            reload();
          }}
        />
      )}
    </>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="sheet-section">
      <h3>{title}</h3>
      {children}
    </section>
  );
}

/** "Prepared by Quotes · 09:31 · Picks the best rate". */
export function ItemProvenance({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const node = producingNode(data);
  if (!node?.finishedAt) return null;
  return (
    <Provenance>
      <CoworkerMark
        coworker={data.item.presentation.coworker}
        size={22}
        decorative
      />
      <span>
        {t("item.preparedBy")} <b>{data.item.presentation.coworker.name}</b> ·{" "}
        {formatTime(node.finishedAt, locale)} ·{" "}
        {localized(node.businessLabel, locale)}
      </span>
    </Provenance>
  );
}

/** The exact content the decision is about, as the producing step left it. */
export function ExactContent({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const node = producingNode(data);
  if (!hasPresentedResult(node)) return null;
  return (
    <Exact label={t("item.exactLabel")}>
      <PresentedResult node={node!} />
    </Exact>
  );
}

function ItemBody({
  data,
  response,
}: {
  data: DetailData;
  response: BlockerResponse;
}) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const session = useSession();
  const { item } = data;
  const [stopping, setStopping] = useState(false);
  const [stopError, setStopError] = useState(false);

  return (
    <>
      <ItemProvenance data={data} />

      {item.state === "blocked" && data.blocker && (
        <div className="decision-card mt-4 flex flex-col gap-3">
          <span className="ib-eyebrow text-warm-ink">
            {data.blocker.kind === "information" ||
            data.blocker.kind === "decision"
              ? t("item.answerNeeded")
              : t("item.okNeeded")}
          </span>
          <h3 className="m-0 text-lg font-semibold">
            {localized(data.blocker.title, locale)}
          </h3>
          <p className="m-0 text-ink-2">
            {localized(data.blocker.description, locale)}
          </p>
          <ExactContent data={data} />
          {response.fields}
        </div>
      )}

      {item.state === "waiting" && item.wait && (
        <Section title={t("item.waitingTitle")}>
          <p className="m-0">
            {t("line.waiting", {
              received: item.wait.received,
              expected: item.wait.expected,
              at: formatDateTime(item.wait.deadlineAt, locale),
            })}
          </p>
          <p className="m-0 mt-1 muted text-sm">
            {item.wait.completion === "any"
              ? t("item.waitAny")
              : t("item.waitAll")}{" "}
            {t("item.nothingNeeded")}
          </p>
          <Button
            className="mt-3"
            size="sm"
            disabled={stopping}
            onClick={async () => {
              setStopping(true);
              setStopError(false);
              try {
                await session.withCSRF((csrf) => stopWaiting(csrf, item));
              } catch (err) {
                if (!session.lost(err)) setStopError(true);
              } finally {
                setStopping(false);
              }
            }}
          >
            {t("item.stopWaiting")}
          </Button>
          <p className="m-0 mt-2 muted text-xs">{t("item.stopWaitingHelp")}</p>
          {stopError && (
            <p role="alert" className="form-error mt-2">
              {t("item.actionFailed")}
            </p>
          )}
        </Section>
      )}

      {item.state === "paused" && (
        <Section title={t("item.pausedTitle")}>
          <p className="m-0">{t(`pause.${item.pause?.reason ?? "other"}`)}</p>
          {item.pause?.since && (
            <p className="m-0 mt-1 muted text-sm">
              {t("item.since", {
                at: formatDateTime(item.pause.since, locale),
              })}
            </p>
          )}
        </Section>
      )}

      {item.state === "failed" && <FailedSection data={data} />}

      {item.state === "done" && <DoneSection data={data} />}

      {(item.state === "in_progress" || item.state === "todo") &&
        item.currentStep && (
          <Section title={t("item.nowTitle")}>
            <p className="m-0">{localized(item.currentStep.label, locale)}</p>
          </Section>
        )}

      <Section title={t("item.timeline")}>
        <Timeline data={data} />
      </Section>

      <Section title={t("item.sources")}>
        <Sources data={data} />
      </Section>
    </>
  );
}

function FailedSection({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const completed = data.nodes.filter(
    (node) =>
      node.attemptId === data.item.currentAttemptId &&
      node.state === "succeeded",
  );
  const unknown = data.consequences.filter(
    (c) => c.state === "outcome_unknown",
  );
  return (
    <>
      <div className="mt-4 flex flex-col gap-2">
        <h3 className="m-0 text-lg font-semibold">
          {t(`failure.${failureCode(data.item)}`)}
        </h3>
        <p className="m-0 text-ink-2">
          {t(`failureHelp.${failureCode(data.item)}`)}
        </p>
      </div>
      <Section title={t("item.whatCompleted")}>
        {completed.length === 0 ? (
          <p className="m-0 muted">{t("item.nothingCompleted")}</p>
        ) : (
          <ul className="m-0 p-0 list-none flex flex-col gap-1">
            {completed.map((node) => (
              <li key={node.id}>
                {localized(node.businessLabel, locale)}{" "}
                <span className="muted text-sm">· {t("item.kept")}</span>
              </li>
            ))}
          </ul>
        )}
      </Section>
      <Section title={t("item.externalActions")}>
        {data.consequences.length === 0 ? (
          <p className="m-0 muted">{t("item.noExternalActions")}</p>
        ) : (
          <ul className="m-0 p-0 list-none flex flex-col gap-1">
            {data.consequences.map((consequence) => (
              <li key={consequence.invocationId}>
                {localized(consequence.summary, locale)}{" "}
                {consequence.state === "outcome_unknown" && (
                  <span className="ib-badge ib-badge--warn">
                    {t("failed.outcomeUnknown")}
                  </span>
                )}
              </li>
            ))}
          </ul>
        )}
        {unknown.length > 0 && (
          <Irreversible>{t("item.checkUnknown")}</Irreversible>
        )}
      </Section>
    </>
  );
}

function DoneSection({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const node = producingNode(data);
  const outcome = data.nodes
    .filter((candidate) =>
      candidate.resultPresentation?.outcomes?.some(
        (o) => o.outcome === candidate.outcome,
      ),
    )
    .at(-1);
  const summary = outcome?.resultPresentation?.outcomes?.find(
    (o) => o.outcome === outcome.outcome,
  )?.summary;
  const approvals = data.timeline.filter(
    (event) => event.code === "blocker_resolved",
  );
  return (
    <div className="mt-4 flex flex-col gap-3">
      <h3 className="m-0 text-lg font-semibold">
        {localized(summary, locale) || t("item.doneSummary")}
      </h3>
      {approvals.map((event) => (
        <p key={event.id} className="m-0 text-sm text-ok-ink">
          {event.actorDisplayName
            ? t("item.approvedBy", {
                name: event.actorDisplayName,
                at: formatTime(event.createdAt, locale),
              })
            : t("item.answeredAt", { at: formatTime(event.createdAt, locale) })}
        </p>
      ))}
      {hasPresentedResult(node) && (
        <Exact label={t("item.exactLabel")}>
          <PresentedResult node={node!} />
        </Exact>
      )}
      <p className="m-0 text-sm text-ink-2">{t("item.doneMeans")}</p>
    </div>
  );
}

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

function Timeline({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const nodes = new Map(data.nodes.map((node) => [node.id, node]));
  const events = data.timeline.filter(
    (event) => !event.code.startsWith("value_"),
  );
  const label = (event: TimelineEvent) => {
    const code = eventCodes.find((known) => known === event.code);
    return code ? t(`event.${code}`) : t("event.work_updated");
  };
  return (
    <ol className="list-none m-0 p-0 flex flex-col gap-2">
      {events.map((event) => {
        const node = event.nodeExecutionId
          ? nodes.get(event.nodeExecutionId)
          : undefined;
        return (
          <li
            key={event.id}
            className="grid grid-cols-[52px_1fr] gap-3 text-sm"
          >
            <time className="ib-num muted" dateTime={event.createdAt}>
              {formatTime(event.createdAt, locale)}
            </time>
            <span>
              {label(event)}
              {node && (
                <span className="muted">
                  {" "}
                  · {localized(node.businessLabel, locale)}
                </span>
              )}
              {event.actorDisplayName && (
                <span className="muted"> · {event.actorDisplayName}</span>
              )}
            </span>
          </li>
        );
      })}
    </ol>
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

function Sources({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const session = useSession();
  const [failed, setFailed] = useState(false);
  const { source } = data.item;
  return (
    <div className="flex flex-col gap-3">
      <div className="ib-card ib-card--pad">
        <span className="ib-eyebrow">
          {t(`source.${sourceKind(source.kind)}`)}
        </span>
        <p className="m-0 mt-1 font-semibold">{source.title}</p>
        {source.subtitle && (
          <p className="m-0 muted text-sm">{source.subtitle}</p>
        )}
        {!!source.evidence?.length && (
          <dl className="m-0 mt-3 grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] gap-x-4 gap-y-1 text-sm">
            {source.evidence.map((field, index) => (
              <div key={index} className="contents">
                <dt className="muted">{localized(field.label, locale)}</dt>
                <dd className="m-0">{field.value}</dd>
              </div>
            ))}
          </dl>
        )}
        {source.originalUrl && (
          <a
            href={source.originalUrl}
            target="_blank"
            rel="noreferrer noopener"
            className="text-link mt-2 inline-block"
          >
            {t("item.openOriginal")}
          </a>
        )}
      </div>
      {data.artifacts.length > 0 && (
        <ul className="list-none m-0 p-0 ib-card ib-divide">
          {data.artifacts.map((artifact) => {
            const kind = artifactKind(artifact);
            return (
              <li
                key={`${artifact.artifactId}-${artifact.role}`}
                className="px-4 py-3 flex items-center gap-3"
              >
                <span className="ib-badge ib-badge--neutral">
                  {t(`artifact.${kind}`)}
                </span>
                <span className="min-w-0 flex-1 truncate">
                  {artifactName(artifact)}
                </span>
                <button
                  type="button"
                  className="text-link"
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
      )}
      {failed && (
        <p role="alert" className="form-error">
          {t("item.artifactFailed")}
        </p>
      )}
    </div>
  );
}

const knownSources = [
  "email",
  "graph_email",
  "outlook",
  "api",
  "schedule",
  "scheduled",
  "chat",
  "artifact",
] as const;
function sourceKind(kind: string): (typeof knownSources)[number] | "other" {
  return knownSources.find((known) => known === kind) ?? "other";
}

function ItemFooter({
  data,
  response,
  onFeedback,
  onThanks,
  onRestart,
}: {
  data: DetailData;
  response: BlockerResponse;
  onFeedback: () => void;
  onThanks: () => void;
  onRestart: () => void;
}) {
  const { t } = useTranslation("work");
  const { item } = data;
  if (item.state === "blocked" && data.blocker)
    return <BlockerFooter response={response} />;
  if (item.state === "done")
    return (
      <footer className="decision-footer">
        <p className="m-0 font-semibold">{t("item.wasThisRight")}</p>
        <div className="grid grid-cols-2 gap-2">
          <Button onClick={onThanks}>{t("yes")}</Button>
          <Button onClick={onFeedback}>{t("item.somethingWrong")}</Button>
        </div>
      </footer>
    );
  if (item.state === "failed")
    return (
      <footer className="decision-footer">
        <Button variant="primary" size="lg" full onClick={onRestart}>
          {t("item.restartWithGuidance")}
        </Button>
        <p className="m-0 muted text-xs">{t("item.restartHelp")}</p>
      </footer>
    );
  return null;
}

function BlockerFooter({ response }: { response: BlockerResponse }) {
  const { t } = useTranslation("work");
  return (
    <footer className="decision-footer">
      {response.error && (
        <p role="alert" className="form-error m-0">
          {response.error}
        </p>
      )}
      <OutcomeButtons response={response} />
      <p className="m-0 muted text-xs">{t("item.decisionTrust")}</p>
    </footer>
  );
}

function FeedbackPanel({
  data,
  onDone,
  onCancel,
}: {
  data: DetailData;
  onDone: (message: string) => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation("work");
  const session = useSession();
  const [category, setCategory] = useState<string>(feedbackCategories[0]);
  const [explanation, setExplanation] = useState("");
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  return (
    <section
      className="sheet-section ib-card ib-card--pad"
      aria-labelledby="feedback-title"
    >
      <h3 id="feedback-title">{t("feedback.title")}</h3>
      <fieldset className="m-0 p-0 border-0 flex flex-wrap gap-2">
        <legend className="sr-only">{t("feedback.category")}</legend>
        {feedbackCategories.map((key) => (
          <Button
            key={key}
            size="sm"
            variant={category === key ? "ink" : "secondary"}
            aria-pressed={category === key}
            onClick={() => setCategory(key)}
          >
            {t(`feedback.${key}`)}
          </Button>
        ))}
      </fieldset>
      <label className="ib-field mt-3">
        <span className="ib-label">{t("feedback.explanation")}</span>
        <textarea
          className="ib-input py-2 min-h-24"
          value={explanation}
          onChange={(event) => setExplanation(event.target.value)}
        />
      </label>
      {failed && (
        <p role="alert" className="form-error mt-2">
          {t("item.actionFailed")}
        </p>
      )}
      <div className="flex gap-2 mt-3">
        <Button
          variant="primary"
          size="sm"
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            setFailed(false);
            try {
              await session.withCSRF((csrf) =>
                saveFeedback(csrf, data.item, category, explanation),
              );
              onDone(t("feedback.saved"));
            } catch (err) {
              if (!session.lost(err)) setFailed(true);
            } finally {
              setBusy(false);
            }
          }}
        >
          {t("feedback.save")}
        </Button>
        <Button size="sm" variant="ghost" onClick={onCancel}>
          {t("cancel")}
        </Button>
      </div>
    </section>
  );
}

/**
 * Restart with guidance: a revised attempt (REQ-024/025). Any external action
 * that may repeat is listed and must be confirmed one by one before it starts.
 */
function RestartDialog({
  data,
  onClose,
  onStarted,
}: {
  data: DetailData;
  onClose: () => void;
  onStarted: () => void;
}) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const session = useSession();
  const [guidance, setGuidance] = useState("");
  const [confirmed, setConfirmed] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const allConfirmed = data.consequences.every((c) =>
    confirmed.includes(c.invocationId),
  );
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <DialogTitle className="m-0 text-lg font-semibold">
          {t("item.restartWithGuidance")}
        </DialogTitle>
        <DialogDescription className="m-0 mt-1 muted text-sm">
          {t("restart.intro")}
        </DialogDescription>
        <label className="ib-field mt-4">
          <span className="ib-label">{t("restart.guidance")}</span>
          <textarea
            className="ib-input py-2 min-h-24"
            value={guidance}
            onChange={(event) => setGuidance(event.target.value)}
          />
        </label>
        {data.consequences.length > 0 && (
          <fieldset className="mt-4 ib-card ib-card--pad flex flex-col gap-2">
            <legend className="font-semibold px-1">
              {t("respond.repeatTitle")}
            </legend>
            <p className="m-0 text-sm text-ink-2">{t("restart.repeatBody")}</p>
            {data.consequences.map((consequence) => (
              <label key={consequence.invocationId} className="ib-check">
                <input
                  type="checkbox"
                  checked={confirmed.includes(consequence.invocationId)}
                  onChange={(event) =>
                    setConfirmed((current) =>
                      event.target.checked
                        ? [...current, consequence.invocationId]
                        : current.filter(
                            (id) => id !== consequence.invocationId,
                          ),
                    )
                  }
                />
                <span className="ib-check__box" aria-hidden="true">
                  <svg viewBox="0 0 12 12" fill="none">
                    <path
                      d="M2.5 6.5l2.2 2L9.5 3.5"
                      stroke="currentColor"
                      strokeWidth="1.8"
                      strokeLinecap="round"
                    />
                  </svg>
                </span>
                {localized(consequence.summary, locale)}
              </label>
            ))}
          </fieldset>
        )}
        {failed && (
          <p role="alert" className="form-error mt-3">
            {t("item.actionFailed")}
          </p>
        )}
        <div className="flex justify-end gap-2 mt-5">
          <Button variant="ghost" onClick={onClose}>
            {t("cancel")}
          </Button>
          <Button
            variant="primary"
            disabled={busy || !guidance.trim() || !allConfirmed}
            onClick={async () => {
              setBusy(true);
              setFailed(false);
              try {
                let feedback = data.feedback.find(
                  (entry) =>
                    entry.attemptId === data.item.currentAttemptId &&
                    !entry.revisedAttemptId,
                );
                if (!feedback)
                  feedback = await session.withCSRF((csrf) =>
                    saveFeedback(
                      csrf,
                      data.item,
                      "wrong_action",
                      guidance.trim(),
                    ),
                  );
                const feedbackId = feedback.id;
                await session.withCSRF((csrf) =>
                  createRevision(
                    csrf,
                    data.item,
                    feedbackId,
                    guidance.trim(),
                    data.consequences,
                  ),
                );
                onStarted();
              } catch (err) {
                if (!session.lost(err)) setFailed(true);
              } finally {
                setBusy(false);
              }
            }}
          >
            {t("restart.start")}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function useItemNotFound(err: unknown): boolean {
  return isNotFound(err);
}
