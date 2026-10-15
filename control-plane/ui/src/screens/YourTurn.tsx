import { Link, useNavigate } from "@tanstack/react-router";
import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useTranslation } from "react-i18next";
import { useLiveQuery } from "../app/live";
import { useSession } from "../app/session";
import { CoworkerMark } from "../components/coworker";
import { ErrorState, Loading } from "../components/states";
import { useLocale, type Locale } from "../i18n";
import { formatNumber, formatTime } from "../i18n/format";
import { listWorkItems, loadDetail } from "../lib/api";
import type { DetailData, WorkItem } from "../lib/types";
import { decisionsOf, firstName, localized } from "../work/model";
import { useBlockerResponse, type BlockerResponse } from "./respond";
import { hasPresentedResult, producingNode } from "./result";
import type { ClearedDecision } from "./today/AllClear";
import {
  CheckIcon,
  CloseIcon,
  DoneCheck,
  InfoIcon,
  LockIcon,
} from "./today/icons";
import { CoworkerRow, ProgressDots, useIsDesk } from "./today/parts";
import { ExactContent, ItemProvenance } from "./WorkItem";

// Your turn (XTDecide, XTPhoneDecide): a focus mode outside the app frame
// that clears decisions one at a time, oldest first, each with its
// provenance and exact content. It ends in All clear on Today. A, E, N and Esc
// mirror visible buttons.

/** A decision is one gate: answering it can open the next gate on the same
 * item, which is a new decision. */
function decisionKey(item: WorkItem): string {
  return item.blocker?.id ?? item.id;
}

export function YourTurn() {
  const navigate = useNavigate();
  const started = useRef(Date.now());
  const [cleared, setCleared] = useState<ClearedDecision[]>([]);
  const [skipped, setSkipped] = useState<string[]>([]);
  // Each answer or skip bumps the generation; All clear waits for a queue read
  // that started after the last one, so a gate the answer opened is seen.
  const generation = useRef(0);
  const [settledAt, setSettledAt] = useState(0);
  // The queue is read live, so a decision someone else clears drops out.
  const queue = useLiveQuery(async () => {
    const read = generation.current;
    return {
      items: await listWorkItems({ state: "blocked" }),
      generation: read,
    };
  }, []);
  const reloadQueue = queue.reload;
  const advance = () => {
    generation.current += 1;
    setSettledAt(generation.current);
    reloadQueue();
  };

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") void navigate({ to: "/" });
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [navigate]);

  const handled = new Set([
    ...cleared.map((entry) => entry.blockerId),
    ...skipped,
  ]);
  const remaining =
    queue.status === "ready"
      ? decisionsOf(queue.data.items).filter(
          (item) => !handled.has(decisionKey(item)),
        )
      : [];
  const current = remaining[0];
  const finished =
    queue.status === "ready" &&
    !current &&
    queue.data.generation >= settledAt &&
    !queue.refreshing;

  // Nothing left: All clear, in the frame with Today current.
  useEffect(() => {
    if (!finished) return;
    void navigate({
      to: "/",
      replace: true,
      state: {
        allClear: {
          cleared,
          skipped: skipped.length,
          seconds: Math.round((Date.now() - started.current) / 1000),
        },
      },
    });
  }, [finished, cleared, skipped.length, navigate]);

  // The read after the last answer failed: the answer was sent, but what is
  // left can't be shown. Say so and retry the read, never the answer.
  const refreshFailed =
    queue.status === "ready" && !current && queue.stale && !queue.refreshing;
  if (queue.status === "error" || refreshFailed)
    return (
      <Focus>
        <ErrorState onRetry={queue.reload} />
      </Focus>
    );
  if (queue.status === "loading" || !current)
    return (
      <Focus>
        <Loading />
      </Focus>
    );

  return (
    <Focus
      position={cleared.length + 1}
      total={cleared.length + remaining.length}
    >
      <Decision
        key={decisionKey(current)}
        item={current}
        upNext={remaining.slice(1, 4)}
        cleared={cleared}
        onCleared={(entry) => {
          setCleared((list) => [...list, entry]);
          advance();
        }}
        onSkip={() => {
          setSkipped((list) => [...list, decisionKey(current)]);
          advance();
        }}
      />
    </Focus>
  );
}

/** XTDecide's own header: Back to Today, progress, "Exact copy kept". */
function Focus({
  position,
  total,
  children,
}: {
  position?: number;
  total?: number;
  children: ReactNode;
}) {
  const { t } = useTranslation("today");
  const desk = useIsDesk();
  const progress = position !== undefined && total !== undefined && (
    <span className="row gap-[10px] desk:gap-[10px] text-[13px] desk:text-[13.5px] text-[var(--ink2)]">
      <ProgressDots position={position} total={total} />
      <span role="status">
        {t(desk ? "turn.progress" : "turn.progressShort", {
          position,
          total,
        })}
      </span>
    </span>
  );
  return (
    <div className="turn">
      {desk ? (
        <header className="turn-head">
          <Link to="/" className="btn btn-ghost btn-sm">
            {t("turn.backToToday")}
            <kbd aria-hidden="true">{t("key.esc", { ns: "app" })}</kbd>
          </Link>
          <span className="mx-auto">{progress}</span>
          <span className="row justify-end gap-[7px] w-[150px] text-[12.5px] text-[var(--ink3)]">
            <LockIcon />
            <span>{t("turn.exactKept")}</span>
          </span>
        </header>
      ) : (
        <header className="turn-head">
          <Link
            to="/"
            className="turn-close"
            aria-label={t("turn.backToToday")}
          >
            <CloseIcon />
          </Link>
          <span className="ml-auto">{progress}</span>
        </header>
      )}
      <main id="main" className="turn-main">
        {children}
      </main>
    </div>
  );
}

function Decision({
  item,
  upNext,
  cleared,
  onCleared,
  onSkip,
}: {
  item: WorkItem;
  upNext: WorkItem[];
  cleared: ClearedDecision[];
  onCleared: (entry: ClearedDecision) => void;
  onSkip: () => void;
}) {
  const { t } = useTranslation("today");
  const locale = useLocale();
  const desk = useIsDesk();
  const session = useSession();
  const me = firstName(session.profile.displayName, session.profile.email);
  const detail = useLiveQuery(() => loadDetail(item.id), [item.id], {
    live: false,
  });
  const data = detail.status === "ready" ? detail.data : undefined;

  const center =
    detail.status === "loading" ? (
      <Loading />
    ) : detail.status === "error" ? (
      <ErrorState onRetry={detail.reload} />
    ) : data?.blocker ? (
      <DecisionCard
        data={data}
        stacked={upNext.length > 0}
        onCleared={onCleared}
        onSkip={onSkip}
      />
    ) : (
      // Someone else answered it meanwhile.
      <div className="card p-6 flex flex-col items-start gap-3">
        <p className="m-0">{t("turn.alreadyAnswered")}</p>
        <button type="button" className="btn btn-secondary" onClick={onSkip}>
          {t("turn.next")}
        </button>
      </div>
    );

  if (!desk) return <div className="turn-phone">{center}</div>;

  return (
    <div className="turn-grid">
      <div className="turn-center">{center}</div>
      <div className="turn-left">
        <div className="row gap-3">
          <CoworkerMark
            coworker={item.presentation.coworker}
            size={48}
            decorative
          />
          <span className="text-[14px]">
            <b>{item.presentation.coworker.name}</b>
            <br />
            <span className="muted">
              {localized(item.presentation.coworker.role, locale)} ·{" "}
              {t("turn.aiCoworker")}
            </span>
          </span>
        </div>
        {data?.blocker && (
          <p className="m-0 text-[16px] leading-[1.5] text-[var(--ink)]">
            {localized(data.blocker.description, locale)}
          </p>
        )}
        {data && <HowItWasBuilt data={data} />}
        {data && <WhatItDid data={data} />}
      </div>
      <div className="turn-right">
        {upNext.length > 0 && (
          <>
            <h2 className="eyebrow m-0">{t("turn.upNext")}</h2>
            <ul className="list flex flex-col gap-[10px]">
              {upNext.map((next) => (
                <li key={next.id} className="card p-[14px]">
                  <CoworkerRow
                    coworker={next.presentation.coworker}
                    state="waiting"
                    size={30}
                    text={localized(next.blocker?.title, locale) || next.title}
                  />
                </li>
              ))}
            </ul>
          </>
        )}
        {cleared.length > 0 && (
          <>
            <h2 className={`eyebrow m-0 ${upNext.length > 0 ? "mt-3" : ""}`}>
              {t("turn.done")}
            </h2>
            <ul className="list flex flex-col gap-[10px]">
              {cleared.map((entry) => (
                <li
                  key={entry.blockerId}
                  className="card row gap-3 p-[14px] opacity-80"
                >
                  <DoneCheck size={26} />
                  <span className="text-[13.5px] leading-[1.4] min-w-0">
                    <b>{entry.title}</b>
                    <br />
                    <span className="muted">
                      {t("turn.answeredBy", {
                        name: me,
                        at: formatTime(entry.at, locale),
                      })}
                    </span>
                  </span>
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
    </div>
  );
}

function DecisionCard({
  data,
  stacked,
  onCleared,
  onSkip,
}: {
  data: DetailData;
  stacked: boolean;
  onCleared: (entry: ClearedDecision) => void;
  onSkip: () => void;
}) {
  const { t } = useTranslation("today");
  const locale = useLocale();
  const desk = useIsDesk();
  const navigate = useNavigate();
  const blocker = data.blocker!;
  const title = localized(blocker.title, locale);
  const response = useBlockerResponse(blocker, (outcome) => {
    onCleared({
      id: data.item.id,
      blockerId: blocker.id,
      title,
      source: data.item.source.title,
      outcome:
        response.outcomes.find((entry) => entry.outcome === outcome)?.label ??
        "",
      at: new Date().toISOString(),
    });
  });
  const responseRef = useRef(response);
  responseRef.current = response;
  const openItem = () =>
    void navigate({ to: "/work/$itemId", params: { itemId: data.item.id } });

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      if (target && /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName)) return;
      if (event.metaKey || event.ctrlKey || event.altKey) return;
      const current = responseRef.current;
      const key = event.key.toLowerCase();
      if (key === "a" && current.outcomes[0]) {
        event.preventDefault();
        void current.submit(current.outcomes[0].outcome);
      } else if (key === "n" && current.outcomes.length > 1) {
        event.preventDefault();
        void current.submit(
          current.outcomes[current.outcomes.length - 1].outcome,
        );
      } else if (key === "e") {
        event.preventDefault();
        void navigate({
          to: "/work/$itemId",
          params: { itemId: data.item.id },
        });
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [navigate, data.item.id]);

  const skip = (
    <button type="button" className="turn-skip" onClick={onSkip}>
      {t("turn.later")}
    </button>
  );
  const error = response.error && (
    <p role="alert" className="form-error m-0">
      {response.error}
    </p>
  );

  return (
    <>
      <div className="relative">
        {desk && stacked && (
          <div className="card turn-stack" aria-hidden="true" />
        )}
        <article
          className="card enter turn-card"
          aria-labelledby="decision-title"
        >
          <ItemProvenance data={data} />
          <h1 id="decision-title" className="turn-title">
            {title}
          </h1>
          {!desk && (
            <p className="m-0 text-[13.5px] leading-[1.45] text-[var(--ink2)]">
              {localized(blocker.description, locale)}
            </p>
          )}
          <span className="muted text-[12.5px] desk:text-[13.5px] leading-[1.4]">
            {data.item.source.title} · {data.item.title}
          </span>
          {desk ? (
            <div className="turn-exact">
              <ExactContent data={data} />
            </div>
          ) : (
            <Clamped>
              <ExactContent data={data} />
            </Clamped>
          )}
          {response.fields}
          {hasPresentedResult(producingNode(data)) && (
            <p className="turn-note">
              <InfoIcon />
              <span>
                {t("turn.exactNote", {
                  name: data.item.presentation.coworker.name,
                })}
              </span>
            </p>
          )}
          {desk && (
            // The decision's own footer: it sticks to the bottom of the
            // window while long exact content scrolls (never off-screen).
            <div className="turn-dock">
              {error}
              <DeskActions
                response={response}
                onOpen={openItem}
                openLabel={t("turn.openItem")}
              />
              {skip}
            </div>
          )}
        </article>
      </div>
      {!desk && <div className="mt-4 text-center">{skip}</div>}
      {!desk && (
        <footer className="turn-foot">
          {error}
          <PhoneActions
            response={response}
            onOpen={openItem}
            openLabel={t("turn.openItem")}
          />
        </footer>
      )}
    </>
  );
}

/** XTDecide's footer: primary (A), Open the item (E), the others (N last). */
function DeskActions({
  response,
  onOpen,
  openLabel,
}: {
  response: BlockerResponse;
  onOpen: () => void;
  openLabel: string;
}) {
  const { t } = useTranslation("app");
  const { outcomes, valid, busy } = response;
  const [first, ...rest] = outcomes;
  const disabled = !valid || busy;
  return (
    <div className="turn-actions">
      {first && (
        <button
          type="button"
          className="btn btn-primary btn-lg"
          disabled={disabled}
          onClick={() => void response.submit(first.outcome)}
        >
          {first.label}
          <kbd aria-hidden="true">{t("key.a")}</kbd>
        </button>
      )}
      <button
        type="button"
        className="btn btn-secondary btn-lg"
        onClick={onOpen}
      >
        {openLabel}
        <kbd aria-hidden="true">{t("key.e")}</kbd>
      </button>
      {rest.map(({ outcome, label }, index) => (
        <button
          key={outcome}
          type="button"
          className="btn btn-secondary btn-lg"
          disabled={disabled}
          onClick={() => void response.submit(outcome)}
        >
          {label}
          {index === rest.length - 1 && (
            <kbd aria-hidden="true">{t("key.n")}</kbd>
          )}
        </button>
      ))}
    </div>
  );
}

/** XTPhoneDecide's fixed footer: the primary full width, the others below. */
function PhoneActions({
  response,
  onOpen,
  openLabel,
}: {
  response: BlockerResponse;
  onOpen: () => void;
  openLabel: string;
}) {
  const { outcomes, valid, busy } = response;
  const [first, ...rest] = outcomes;
  const disabled = !valid || busy;
  return (
    <>
      {first && (
        <button
          type="button"
          className="btn btn-primary btn-lg btn-full"
          disabled={disabled}
          onClick={() => void response.submit(first.outcome)}
        >
          {first.label}
        </button>
      )}
      <div className="turn-foot__row">
        <button type="button" className="btn btn-secondary" onClick={onOpen}>
          {openLabel}
        </button>
        {rest.map(({ outcome, label }) => (
          <button
            key={outcome}
            type="button"
            className="btn btn-secondary"
            disabled={disabled}
            onClick={() => void response.submit(outcome)}
          >
            {label}
          </button>
        ))}
      </div>
    </>
  );
}

/** The exact content, clamped on phone with "Read all of it" (XTPhoneDecide). */
function Clamped({ children }: { children: ReactNode }) {
  const { t } = useTranslation("today");
  const box = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [overflows, setOverflows] = useState(false);
  useLayoutEffect(() => {
    const element = box.current;
    if (element) setOverflows(element.scrollHeight > element.clientHeight + 2);
  }, [children]);
  return (
    <>
      <div
        ref={box}
        className={`turn-exact turn-exact--phone${open ? "" : " turn-exact--clamped"}`}
      >
        {children}
      </div>
      {(overflows || open) && (
        <button
          type="button"
          className="turn-more"
          aria-expanded={open}
          onClick={() => setOpen((value) => !value)}
        >
          {open ? t("turn.readLess") : t("turn.readAll")}
        </button>
      )}
    </>
  );
}

interface BuiltRow {
  key: string;
  label: string;
  value: string;
  numeric: boolean;
}

/**
 * "How it was built": the producing step's presented fields when it has
 * them, otherwise the work's source evidence, otherwise nothing.
 */
function builtRows(
  data: DetailData,
  locale: Locale,
  yesNo: (value: boolean) => string,
): BuiltRow[] {
  const node = producingNode(data);
  if (hasPresentedResult(node)) {
    const output = node!.output as Record<string, unknown>;
    return (node!.resultPresentation?.fields ?? []).flatMap((field) => {
      if (field.path.length !== 1) return [];
      const key = field.path[0];
      if (!Object.prototype.hasOwnProperty.call(output, key)) return [];
      const raw = output[key];
      if (raw === null || typeof raw === "object") return [];
      const option = field.options?.find((entry) => entry.value === raw);
      const value = option
        ? localized(option.label, locale)
        : typeof raw === "boolean"
          ? yesNo(raw)
          : typeof raw === "number"
            ? formatNumber(raw, locale)
            : String(raw);
      return [
        {
          key,
          label: localized(field.label, locale),
          value,
          numeric: typeof raw === "number" && !option,
        },
      ];
    });
  }
  return (data.item.source.evidence ?? []).map((entry, index) => ({
    key: String(index),
    label: localized(entry.label, locale),
    value: entry.value,
    numeric: false,
  }));
}

function HowItWasBuilt({ data }: { data: DetailData }) {
  const { t } = useTranslation("today");
  const locale = useLocale();
  const rows = builtRows(data, locale, (value) =>
    value ? t("yes", { ns: "work" }) : t("no", { ns: "work" }),
  );
  if (rows.length === 0) return null;
  return (
    <section className="card px-4 py-[14px]" aria-labelledby="turn-built">
      <h2 id="turn-built" className="eyebrow m-0">
        {t("turn.howBuilt")}
      </h2>
      <dl className="turn-built">
        {rows.map((row) => (
          <div key={row.key} className="contents">
            <dt className="muted">{row.label}</dt>
            <dd className={row.numeric ? "mono" : undefined}>{row.value}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

/** "What Quotes did": the steps that finished on the way to this decision. */
function WhatItDid({ data }: { data: DetailData }) {
  const { t } = useTranslation("today");
  const locale = useLocale();
  const labels = [
    ...new Set(
      data.nodes
        .filter(
          (node) =>
            node.attemptId === data.item.currentAttemptId &&
            node.state === "succeeded" &&
            node.kind === "agent_task",
        )
        .sort((a, b) => a.executionSeq - b.executionSeq)
        .map((node) => localized(node.businessLabel, locale))
        .filter(Boolean),
    ),
  ];
  if (labels.length === 0) return null;
  return (
    <section className="card px-4 py-[14px]" aria-labelledby="turn-did">
      <h2 id="turn-did" className="eyebrow m-0">
        {t("turn.whatItDid", { name: data.item.presentation.coworker.name })}
      </h2>
      <ul className="list mt-[6px]">
        {labels.map((label) => (
          <li
            key={label}
            className="row gap-[9px] items-start py-[5px] text-[13.5px]"
          >
            <CheckIcon />
            <span>{label}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
