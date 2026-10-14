import { Link } from "@tanstack/react-router";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useLiveQuery } from "../app/live";
import { isNotFound, useSession } from "../app/session";
import { CoworkerMark } from "../components/coworker";
import { ErrorState, Loading, StaleNotice } from "../components/states";
import { Exact, Irreversible } from "../components/trust";
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
  createRevision,
  loadDetail,
  saveFeedback,
  stopWaiting,
} from "../lib/api";
import type { DetailData, WorkItem } from "../lib/types";
import { failureCode, localized } from "../work/model";
import {
  Breakdowns,
  ExactRows,
  hasPresentedResult,
  PresentedResult,
  producingNode,
} from "./result";
import {
  OutcomeButtons,
  useBlockerResponse,
  type BlockerResponse,
} from "./respond";
import {
  CheckIcon,
  ChevronLeft,
  CloseIcon,
  DotsIcon,
  LockIcon,
  ShieldIcon,
} from "./work/icons";
import { Sources, Timeline } from "./work/itemParts";

// The work item (XTItem, XTItemQuestion, XTItemFailed, XTItemDone and their
// phone boards): status and provenance, the action first, the timeline,
// sources and documents, and the decision in a fixed footer. It opens as a
// side sheet over Work (full screen on phone, with its own "‹ Work" bar);
// /work/<id> is also its deep link.

export function WorkItemSheet({
  itemId,
  onClose,
}: {
  itemId: string;
  onClose: () => void;
}) {
  const content = useRef<HTMLDivElement>(null);
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <SheetContent
        ref={content}
        className="xt work-sheet"
        aria-describedby={undefined}
        onOpenAutoFocus={(event) => {
          // Focus the sheet itself, so Enter can mirror "Review and decide"
          // instead of opening the first control (the ⋯ menu).
          event.preventDefault();
          content.current?.focus();
        }}
      >
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

function isQuestion(kind: string | undefined): boolean {
  return kind === "information" || kind === "decision";
}

function WorkItemView({ itemId }: { itemId: string }) {
  const { t } = useTranslation("work");
  const detail = useLiveQuery(() => loadDetail(itemId), [itemId]);

  if (detail.status !== "ready")
    return (
      <>
        <header className="item-head">
          <div className="item-head__bar">
            <BackToWork />
            <DialogClose className="item-close" aria-label={t("item.close")}>
              <CloseIcon />
            </DialogClose>
          </div>
          <DialogTitle className="item-head__title">
            {detail.status === "loading"
              ? t("item.loadingTitle")
              : t("item.errorTitle")}
          </DialogTitle>
        </header>
        <div className="item-body">
          {detail.status === "loading" ? (
            <Loading />
          ) : (
            <ErrorState onRetry={detail.reload} />
          )}
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

function BackToWork() {
  const { t } = useTranslation("work");
  return (
    <DialogClose className="phone-back item-back" aria-label={t("item.back")}>
      <ChevronLeft />
      {t("item.backShort")}
    </DialogClose>
  );
}

function interactive(target: EventTarget | null): boolean {
  return (
    target instanceof Element &&
    !!target.closest(
      "a, button, input, textarea, select, [role=menu], [role=menuitem]",
    )
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
  const [panel, setPanel] = useState<"none" | "feedback" | "restart">("none");
  const [notice, setNotice] = useState("");
  const [reviewing, setReviewing] = useState(false);
  const decision = useRef<HTMLElement>(null);
  const { item } = data;
  const blocker = item.state === "blocked" ? data.blocker : null;
  const response = useBlockerResponse(blocker, () => {
    setNotice(t("item.responseSent"));
    setReviewing(false);
    reload();
  });
  const needsReview = !!blocker && response.quick && !response.choice;

  // "Review and decide" brings the decision card into view and puts the
  // decision buttons in the footer; Enter mirrors the visible button.
  const review = () => setReviewing(true);
  useEffect(() => {
    if (reviewing) {
      decision.current?.scrollIntoView?.({ block: "nearest" });
      decision.current?.focus();
    }
  }, [reviewing]);
  useEffect(() => {
    if (!needsReview || reviewing) return;
    const onKey = (event: KeyboardEvent) => {
      if (
        event.key === "Enter" &&
        !event.metaKey &&
        !event.ctrlKey &&
        !event.altKey &&
        !event.shiftKey &&
        !interactive(event.target)
      ) {
        event.preventDefault();
        setReviewing(true);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [needsReview, reviewing]);

  return (
    <>
      <div className="item-scroll">
        <ItemHeader item={item} onFeedback={() => setPanel("feedback")} />
        <div className="item-body">
          {stale && <StaleNotice onRetry={reload} />}
          {notice && (
            <p role="status" className="item-notice">
              {notice}
            </p>
          )}
          <ItemBody data={data} response={response} decision={decision} />
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
      </div>

      <ItemFooter
        data={data}
        response={response}
        reviewing={reviewing || !needsReview}
        onReview={review}
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

const emailKinds = ["email", "graph_email", "outlook"];
const scheduleKinds = ["schedule", "scheduled"];

/** Badge, id, ⋯ and close; the title; the workflow and where it came from. */
function ItemHeader({
  item,
  onFeedback,
}: {
  item: WorkItem;
  onFeedback: () => void;
}) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const { source } = item;
  const schedule = scheduleKinds.includes(source.kind);
  return (
    <header className="item-head">
      <div className="item-head__bar">
        <BackToWork />
        <span className="mono muted item-head__id">{item.id}</span>
        <DropdownMenu>
          <DropdownMenuTrigger
            className="item-icon item-icon--menu"
            aria-label={t("item.moreActions")}
          >
            <DotsIcon />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-[210px]">
            <DropdownMenuItem onSelect={onFeedback}>
              {t("item.giveFeedback")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
        <DialogClose
          className="item-icon item-close"
          aria-label={t("item.close")}
        >
          <CloseIcon />
        </DialogClose>
      </div>
      <span className="item-head__badge">
        <StateBadge item={item} />
      </span>
      <DialogTitle className="item-head__title">{item.title}</DialogTitle>
      <p className="item-head__source">
        <Link
          to="/team/$key"
          params={{ key: item.workflowKey }}
          className="desk:underline"
        >
          {localized(item.presentation.title, locale)}
        </Link>{" "}
        · {t("item.byCoworker", { name: item.presentation.coworker.name })} ·{" "}
        {schedule
          ? source.title
          : emailKinds.includes(source.kind)
            ? t("item.fromEmail", { from: source.title })
            : t("item.from", { source: source.title })}
        {!schedule && (
          <span className="hidden desk:inline">
            {" "}
            · {formatTime(item.createdAt, locale)}
          </span>
        )}
      </p>
    </header>
  );
}

/** "Prepared by Quotes · 09:31 · Picks the best rate". */
export function ItemProvenance({
  data,
  children,
}: {
  data: DetailData;
  children?: ReactNode;
}) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const node = producingNode(data);
  if (!node?.finishedAt) return null;
  return (
    <p className="item-provenance">
      <CoworkerMark
        coworker={data.item.presentation.coworker}
        size={28}
        decorative
      />
      <span className="min-w-0">
        {t("item.preparedBy")} <b>{data.item.presentation.coworker.name}</b> ·{" "}
        <span className="mono">{formatTime(node.finishedAt, locale)}</span> ·{" "}
        {localized(node.businessLabel, locale)}
        {children}
      </span>
    </p>
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
  decision,
}: {
  data: DetailData;
  response: BlockerResponse;
  decision: React.RefObject<HTMLElement | null>;
}) {
  const { item } = data;
  return (
    <>
      {item.state === "blocked" && data.blocker && (
        <DecisionBody data={data} response={response} decision={decision} />
      )}
      {item.state === "waiting" && item.wait && <WaitingCard item={item} />}
      {item.state === "paused" && <PausedCard item={item} />}
      {(item.state === "in_progress" || item.state === "todo") && (
        <NowCard data={data} />
      )}
      {item.state === "failed" && <FailedBody data={data} />}
      {item.state === "done" && <DoneBody data={data} />}

      {item.state !== "done" && <Timeline data={data} />}
      <Sources data={data} />
      {item.state === "done" && <DoneMeans />}
    </>
  );
}

function DecisionBody({
  data,
  response,
  decision,
}: {
  data: DetailData;
  response: BlockerResponse;
  decision: React.RefObject<HTMLElement | null>;
}) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const blocker = data.blocker!;
  const node = producingNode(data);
  const exact = hasPresentedResult(node) ? node! : undefined;
  const description = localized(blocker.description, locale);
  const fields = response.choice ? null : response.fields;

  if (isQuestion(blocker.kind))
    return (
      <>
        <ItemProvenance data={data} />
        <section
          ref={decision}
          tabIndex={-1}
          className="flex flex-col gap-2 outline-none"
          aria-labelledby="decision-title"
        >
          <span className="eyebrow">{t("item.answerNeeded")}</span>
          <h3 id="decision-title" className="m-0 text-[18px] font-semibold">
            {localized(blocker.title, locale)}
          </h3>
          {description && (
            <p className="m-0 text-[14px] leading-[1.55] text-[var(--ink2)]">
              {description}
            </p>
          )}
          {fields && <div className="card px-4 py-[14px]">{fields}</div>}
        </section>
        {exact && (
          <figure className="card exact-figure">
            <ExactRows node={exact} />
          </figure>
        )}
        {exact && <Breakdowns node={exact} />}
      </>
    );

  return (
    <>
      <ItemProvenance data={data} />
      <section
        ref={decision}
        tabIndex={-1}
        className="card decision-card"
        aria-labelledby="decision-title"
      >
        <span className="eyebrow decision-card__eyebrow">
          {t("item.okNeeded")}
        </span>
        <b
          id="decision-title"
          className="text-[15px] desk:text-[15.5px] font-semibold"
        >
          {localized(blocker.title, locale)}
        </b>
        {description && (
          <p className="m-0 text-[13px] desk:text-[13.5px] leading-[1.5] text-[var(--ink2)]">
            {description}
          </p>
        )}
        {exact && <ExactRows node={exact} />}
        {fields}
      </section>
      {exact && <Breakdowns node={exact} />}
    </>
  );
}

function StateCard({
  title,
  children,
}: {
  title: string;
  children: ReactNode;
}) {
  return (
    <section className="card item-state-card">
      <h3 className="eyebrow m-0">{title}</h3>
      {children}
    </section>
  );
}

function WaitingCard({ item }: { item: WorkItem }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const session = useSession();
  const [stopping, setStopping] = useState(false);
  const [stopError, setStopError] = useState(false);
  const wait = item.wait!;
  return (
    <StateCard title={t("item.waitingTitle")}>
      <p className="m-0 text-[14px]">
        {t("line.waiting", {
          received: wait.received,
          expected: wait.expected,
          at: formatDateTime(wait.deadlineAt, locale),
        })}
      </p>
      <p className="m-0 text-[13px] text-[var(--ink3)]">
        {wait.completion === "any" ? t("item.waitAny") : t("item.waitAll")}{" "}
        {t("item.nothingNeeded")}
      </p>
      <button
        type="button"
        className="btn btn-secondary btn-sm self-start mt-1"
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
      </button>
      <p className="m-0 text-[12.5px] text-[var(--ink3)]">
        {t("item.stopWaitingHelp")}
      </p>
      {stopError && (
        <p role="alert" className="form-error m-0">
          {t("item.actionFailed")}
        </p>
      )}
    </StateCard>
  );
}

function PausedCard({ item }: { item: WorkItem }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  return (
    <StateCard title={t("item.pausedTitle")}>
      <p className="m-0 text-[14px]">
        {t(`pause.${item.pause?.reason ?? "other"}`)}
      </p>
      {item.pause?.since && (
        <p className="m-0 text-[13px] text-[var(--ink3)]">
          {t("item.since", { at: formatDateTime(item.pause.since, locale) })}
        </p>
      )}
    </StateCard>
  );
}

/** Working items lead with provenance; the current step closes the timeline. */
function NowCard({ data }: { data: DetailData }) {
  return <ItemProvenance data={data} />;
}

function FailedBody({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const { item } = data;
  const coworker = item.presentation.coworker;
  const code = failureCode(item);
  const steps = data.nodes
    .filter(
      (node) =>
        node.attemptId === item.currentAttemptId &&
        (node.state === "succeeded" ||
          node.state === "failed" ||
          node.state === "interrupted"),
    )
    .sort((a, b) => a.executionSeq - b.executionSeq);
  const kept = steps.filter((node) => node.state === "succeeded").length;
  const unknown = data.consequences.some((c) => c.state === "outcome_unknown");
  return (
    <>
      <div className="row items-start gap-3 min-w-0">
        <CoworkerMark coworker={coworker} size={36} state="failed" decorative />
        <div className="flex flex-col gap-[2px] min-w-0">
          <span className="row flex-wrap gap-[7px]">
            <b className="text-[15px] font-semibold">{coworker.name}</b>
            <span className="muted text-[12.5px]">
              {localized(coworker.role, locale)}
            </span>
            <span className="dot bg-[var(--crit)]" aria-hidden="true" />
            <span className="text-[12.5px] text-[var(--crit-ink)]">
              {t("item.failedWord")}
            </span>
          </span>
          <span className="text-[13.5px] leading-[1.4] text-[var(--ink2)]">
            {t("item.couldntFinish")}
            {data.consequences.length === 0 && <> {t("item.nothingSent")}</>}
          </span>
        </div>
      </div>
      <section className="flex flex-col gap-2">
        <h3 className="m-0 text-[18px] font-semibold">
          {t(`failure.${code}`)}
        </h3>
        <p className="m-0 text-[14px] leading-[1.55] text-[var(--ink2)]">
          {t(`failureHelp.${code}`)}
        </p>
      </section>
      <section className="card px-4 py-3">
        <div className="row gap-2">
          <h3 className="eyebrow m-0">{t("item.whatCompleted")}</h3>
          <span className="muted ml-auto text-[12.5px]">
            {t("item.stepsKept", { count: kept })}
          </span>
        </div>
        {steps.length === 0 ? (
          <p className="m-0 mt-2 text-[13.5px] text-[var(--ink3)]">
            {t("item.nothingCompleted")}
          </p>
        ) : (
          <ul className="list divide mt-1">
            {steps.map((node) => (
              <li
                key={node.id}
                className={`row gap-[10px] py-[7px] text-[13.5px] ${node.state === "succeeded" ? "" : "text-[var(--ink3)]"}`}
              >
                {node.state === "succeeded" ? (
                  <CheckIcon />
                ) : (
                  <span className="w-[15px] text-center" aria-hidden="true">
                    —
                  </span>
                )}
                <span className="min-w-0">
                  {localized(node.businessLabel, locale)}
                </span>
                <span className="mono ml-auto whitespace-nowrap">
                  {node.state === "succeeded"
                    ? t("item.kept")
                    : t("item.notFinished")}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
      {data.consequences.length > 0 && (
        <section className="card px-4 py-3">
          <h3 className="eyebrow m-0">{t("item.externalActions")}</h3>
          <ul className="list divide mt-1">
            {data.consequences.map((consequence) => (
              <li
                key={consequence.invocationId}
                className="row flex-wrap gap-[10px] py-[7px] text-[13.5px]"
              >
                <span className="min-w-0">
                  {localized(consequence.summary, locale)}
                </span>
                {consequence.state === "outcome_unknown" && (
                  <span className="badge ml-auto bg-[var(--warn2)] text-[var(--warn-ink)]">
                    {t("failed.outcomeUnknown")}
                  </span>
                )}
              </li>
            ))}
          </ul>
          {unknown && (
            <div className="mt-2">
              <Irreversible>{t("item.checkUnknown")}</Irreversible>
            </div>
          )}
        </section>
      )}
      <p className="m-0 text-[12.5px] desk:text-[13px] leading-[1.5] text-[var(--ink3)]">
        {t("restart.intro")}
      </p>
    </>
  );
}

function DoneBody({ data }: { data: DetailData }) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const { item } = data;
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
  const exact = hasPresentedResult(node) ? node! : undefined;
  return (
    <>
      <section className="card done-card">
        <div className="row gap-[10px]">
          <span className="done-card__check" aria-hidden="true">
            <CheckIcon size={11} width={3} stroke="var(--ok-ink)" />
          </span>
          <h3 className="m-0 text-[14.5px] desk:text-[16px] font-semibold">
            {localized(summary, locale) || t("item.doneSummary")}
          </h3>
        </div>
        {item.finishedAt && (
          <p className="m-0 text-[12.5px] desk:text-[13.5px] leading-[1.5] text-[var(--ink2)]">
            {t("item.finishedAt")}{" "}
            <span className="mono">{formatTime(item.finishedAt, locale)}</span>
          </p>
        )}
      </section>
      <ItemProvenance data={data}>
        {approvals.map((event) => (
          <span key={event.id}>
            {" "}
            ·{" "}
            {event.actorDisplayName ? (
              <>
                {t("item.answeredBy")} <b>{event.actorDisplayName}</b>
              </>
            ) : (
              t("item.answered")
            )}{" "}
            ·{" "}
            <span className="mono">{formatTime(event.createdAt, locale)}</span>
          </span>
        ))}
      </ItemProvenance>
      {!node?.finishedAt &&
        approvals.map((event) => (
          <p key={event.id} className="m-0 text-[13px] text-[var(--ink2)]">
            {event.actorDisplayName ? (
              <>
                {t("item.answeredBy")} <b>{event.actorDisplayName}</b>
              </>
            ) : (
              t("item.answered")
            )}{" "}
            ·{" "}
            <span className="mono">{formatTime(event.createdAt, locale)}</span>
          </p>
        ))}
      {exact && (
        <section className="flex flex-col gap-[6px]">
          <div className="row gap-2">
            <h3 className="eyebrow m-0">{t("item.exactKept")}</h3>
            {exact.finishedAt && (
              <span className="muted ml-auto text-[12.5px]">
                {t("item.copyKeptAt", {
                  at: formatTime(exact.finishedAt, locale),
                })}
              </span>
            )}
          </div>
          <div className="card exact-figure">
            <ExactRows node={exact} />
          </div>
        </section>
      )}
      {exact && <Breakdowns node={exact} />}
      <Timeline data={data} />
    </>
  );
}

function DoneMeans() {
  const { t } = useTranslation("work");
  return (
    <p className="m-0 text-[12.5px] leading-[1.45] text-[var(--ink3)]">
      {t("item.doneMeans")}
    </p>
  );
}

function TrustNote({
  icon = "lock",
  children,
}: {
  icon?: "lock" | "shield";
  children: ReactNode;
}) {
  return (
    <span className="row gap-[7px] text-[12px] desk:text-[12.5px] text-[var(--ink3)]">
      {icon === "lock" ? <LockIcon /> : <ShieldIcon />}
      <span>{children}</span>
    </span>
  );
}

function ItemFooter({
  data,
  response,
  reviewing,
  onReview,
  onFeedback,
  onThanks,
  onRestart,
}: {
  data: DetailData;
  response: BlockerResponse;
  reviewing: boolean;
  onReview: () => void;
  onFeedback: () => void;
  onThanks: () => void;
  onRestart: () => void;
}) {
  const { t } = useTranslation("work");
  const { item } = data;
  if (item.state === "blocked" && data.blocker)
    return (
      <BlockerFooter
        question={isQuestion(data.blocker.kind)}
        response={response}
        reviewing={reviewing}
        onReview={onReview}
      />
    );
  if (item.state === "done")
    return (
      <footer className="item-foot">
        <b className="text-[14px] font-semibold">{t("item.wasThisRight")}</b>
        <div className="grid grid-cols-2 gap-2 desk:gap-[10px]">
          <button
            type="button"
            className="btn btn-secondary item-foot__btn"
            onClick={onThanks}
          >
            {t("yes")}
          </button>
          <button
            type="button"
            className="btn btn-secondary item-foot__btn px-[10px]"
            onClick={onFeedback}
          >
            {t("item.somethingWrong")}
          </button>
        </div>
        <span className="hidden desk:block muted text-[12.5px]">
          {t("item.feedbackHelps", {
            name: item.presentation.coworker.name,
          })}
        </span>
      </footer>
    );
  if (item.state === "failed")
    return (
      <footer className="item-foot">
        <button
          type="button"
          className="btn btn-primary btn-lg btn-full"
          onClick={onRestart}
        >
          {t("item.restartWithGuidance")}
        </button>
        <span className="row justify-center">
          <TrustNote icon="shield">{t("item.restartHelp")}</TrustNote>
        </span>
      </footer>
    );
  return null;
}

function BlockerFooter({
  question,
  response,
  reviewing,
  onReview,
}: {
  question: boolean;
  response: BlockerResponse;
  reviewing: boolean;
  onReview: () => void;
}) {
  const { t } = useTranslation("work");
  const trust = question ? (
    t("item.answerTrust")
  ) : (
    <>
      <span className="desk:hidden">{t("item.decisionTrustShort")}</span>
      <span className="hidden desk:inline">{t("item.decisionTrust")}</span>
    </>
  );
  const error = response.error && (
    <p role="alert" className="form-error m-0">
      {response.error}
    </p>
  );
  const { choice } = response;
  if (choice)
    return (
      <footer className="item-foot">
        {error}
        <b className="text-[14px] desk:text-[13.5px] font-semibold">
          {choice.label}
        </b>
        {choice.options.map((option) => (
          <button
            key={option.draft}
            type="button"
            className="card choice-btn"
            disabled={response.busy}
            onClick={() =>
              void response.submit(choice.outcome, {
                [choice.key]: option.draft,
              })
            }
          >
            <b className="mono text-[14px]">{option.label}</b>
          </button>
        ))}
        <div className="row flex-wrap gap-[10px]">
          {choice.others.map((other) => (
            <button
              key={other.outcome}
              type="button"
              className="btn btn-ghost btn-sm choice-other"
              disabled={!response.valid || response.busy}
              onClick={() => void response.submit(other.outcome)}
            >
              {other.label}
            </button>
          ))}
          <span className="hidden desk:flex ml-auto">
            <TrustNote>{trust}</TrustNote>
          </span>
        </div>
      </footer>
    );
  if (!reviewing)
    return (
      <footer className="item-foot">
        {error}
        <button
          type="button"
          className="btn btn-primary btn-lg btn-full"
          onClick={onReview}
        >
          {t("item.reviewDecide")}
          <kbd aria-hidden="true">{t("item.enterKey")}</kbd>
        </button>
        <span className="row justify-center">
          <TrustNote>{trust}</TrustNote>
        </span>
      </footer>
    );
  return (
    <footer className="item-foot">
      {error}
      <OutcomeButtons response={response} />
      <span className="row justify-center">
        <TrustNote>{trust}</TrustNote>
      </span>
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
  const panel = useRef<HTMLElement>(null);
  const [category, setCategory] = useState<string>(feedbackCategories[0]);
  const [explanation, setExplanation] = useState("");
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    panel.current?.scrollIntoView?.({ block: "nearest" });
  }, []);
  return (
    <section
      ref={panel}
      className="card item-state-card"
      aria-labelledby="feedback-title"
    >
      <h3 id="feedback-title" className="m-0 text-[15px] font-semibold">
        {t("feedback.title")}
      </h3>
      <fieldset className="m-0 p-0 border-0 flex flex-wrap gap-2">
        <legend className="sr-only">{t("feedback.category")}</legend>
        {feedbackCategories.map((key) => (
          <button
            key={key}
            type="button"
            className={`btn btn-sm ${category === key ? "btn-ink" : "btn-secondary"}`}
            aria-pressed={category === key}
            onClick={() => setCategory(key)}
          >
            {t(`feedback.${key}`)}
          </button>
        ))}
      </fieldset>
      <label className="ib-field">
        <span className="ib-label">{t("feedback.explanation")}</span>
        <textarea
          className="ib-input py-2 min-h-24"
          value={explanation}
          onChange={(event) => setExplanation(event.target.value)}
        />
      </label>
      {failed && (
        <p role="alert" className="form-error m-0">
          {t("item.actionFailed")}
        </p>
      )}
      <div className="flex gap-2">
        <button
          type="button"
          className="btn btn-primary btn-sm"
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
        </button>
        <button
          type="button"
          className="btn btn-ghost btn-sm"
          onClick={onCancel}
        >
          {t("cancel")}
        </button>
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
      <DialogContent className="xt">
        <DialogTitle className="m-0 text-[18px] font-semibold">
          {t("item.restartWithGuidance")}
        </DialogTitle>
        <DialogDescription className="m-0 mt-1 text-[13.5px] text-[var(--ink2)]">
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
          <fieldset className="mt-4 card px-4 py-3 flex flex-col gap-2">
            <legend className="font-semibold px-1">
              {t("respond.repeatTitle")}
            </legend>
            <p className="m-0 text-[13.5px] text-[var(--ink2)]">
              {t("restart.repeatBody")}
            </p>
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
          <button type="button" className="btn btn-ghost" onClick={onClose}>
            {t("cancel")}
          </button>
          <button
            type="button"
            className="btn btn-primary"
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
          </button>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function useItemNotFound(err: unknown): boolean {
  return isNotFound(err);
}
