import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useLiveQuery } from "../app/live";
import { useSession } from "../app/session";
import { CheckIcon } from "../components/brand";
import { CoworkerLine, CoworkerMark } from "../components/coworker";
import { ErrorState, Loading } from "../components/states";
import { Irreversible, TrustLine } from "../components/trust";
import { Button, Kbd } from "../components/ui/button";
import { useLocale } from "../i18n";
import { formatTime } from "../i18n/format";
import { listWorkItems, loadDetail } from "../lib/api";
import type { DetailData, WorkItem } from "../lib/types";
import { decisionsOf, localized, nextDeadline } from "../work/model";
import { OutcomeButtons, useBlockerResponse } from "./respond";
import { hasPresentedResult, producingNode } from "./result";
import { ExactContent, ItemProvenance } from "./WorkItem";

// Your turn: a focus mode that clears decisions one at a time, each with its
// provenance and exact content, ending in All clear (HOR-459 Design → Your
// turn). Enter, A, E and N mirror visible buttons.

interface Cleared {
  item: WorkItem;
  outcomeLabel: string;
  at: Date;
}

export function YourTurn() {
  const { t } = useTranslation("work");
  const navigate = useNavigate();
  const started = useRef(new Date());
  const [cleared, setCleared] = useState<Cleared[]>([]);
  const [skipped, setSkipped] = useState<string[]>([]);
  // The queue is read live, so a decision someone else clears drops out.
  const queue = useLiveQuery(() => listWorkItems({ state: "blocked" }), []);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") void navigate({ to: "/" });
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [navigate]);

  if (queue.status === "loading")
    return (
      <Focus>
        <Loading />
      </Focus>
    );
  if (queue.status === "error")
    return (
      <Focus>
        <ErrorState onRetry={queue.reload} />
      </Focus>
    );

  const clearedIds = new Set(cleared.map((entry) => entry.item.id));
  const remaining = decisionsOf(queue.data).filter(
    (item) => !clearedIds.has(item.id) && !skipped.includes(item.id),
  );
  const total = cleared.length + remaining.length;
  const current = remaining[0];

  if (!current)
    return (
      <AllClear
        cleared={cleared}
        startedAt={started.current}
        skipped={skipped.length}
      />
    );

  return (
    <Focus
      progress={t("turn.progress", { position: cleared.length + 1, total })}
    >
      <Decision
        key={current.id}
        item={current}
        upNext={remaining.slice(1, 4)}
        cleared={cleared}
        onCleared={(outcomeLabel) =>
          setCleared((list) => [
            ...list,
            { item: current, outcomeLabel, at: new Date() },
          ])
        }
        onSkip={() => setSkipped((list) => [...list, current.id])}
      />
    </Focus>
  );
}

function Focus({
  progress,
  children,
}: {
  progress?: string;
  children: React.ReactNode;
}) {
  const { t } = useTranslation("work");
  return (
    <div className="app">
      <header className="flex items-center gap-4 h-14 desk:h-[68px] px-4 desk:px-10 border-b border-line-faint">
        <Link to="/" className="text-link inline-flex items-center gap-2">
          {t("turn.backToToday")}
          <Kbd>{t("key.esc", { ns: "app" })}</Kbd>
        </Link>
        {progress && (
          <span className="ib-num muted text-sm ml-auto" role="status">
            {progress}
          </span>
        )}
      </header>
      <main id="main" className="app-main max-w-[1100px]">
        {children}
      </main>
      <footer className="trust-footer">
        <TrustLine />
      </footer>
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
  cleared: Cleared[];
  onCleared: (outcomeLabel: string) => void;
  onSkip: () => void;
}) {
  const { t } = useTranslation("work");
  const detail = useLiveQuery(() => loadDetail(item.id), [item.id], {
    live: false,
  });
  return (
    <div className="grid gap-8 desk:grid-cols-[minmax(0,1fr)_260px]">
      <div className="min-w-0">
        {detail.status === "loading" && <Loading />}
        {detail.status === "error" && <ErrorState onRetry={detail.reload} />}
        {detail.status === "ready" &&
          (detail.data.blocker ? (
            <DecisionCard
              data={detail.data}
              onCleared={onCleared}
              onSkip={onSkip}
            />
          ) : (
            // Someone else answered it meanwhile.
            <div className="ib-card ib-card--pad flex flex-col gap-3">
              <p className="m-0">{t("turn.alreadyAnswered")}</p>
              <Button onClick={onSkip}>{t("turn.next")}</Button>
            </div>
          ))}
      </div>
      <aside className="flex flex-col gap-6" aria-label={t("turn.sideLabel")}>
        {upNext.length > 0 && (
          <section>
            <h2 className="ib-eyebrow m-0 mb-2">{t("turn.upNext")}</h2>
            <ul className="list-none m-0 p-0 flex flex-col gap-3">
              {upNext.map((next) => (
                <li key={next.id}>
                  <CoworkerLine
                    coworker={next.presentation.coworker}
                    size={28}
                    text={next.title}
                  />
                </li>
              ))}
            </ul>
          </section>
        )}
        {cleared.length > 0 && (
          <section>
            <h2 className="ib-eyebrow m-0 mb-2">{t("turn.done")}</h2>
            <ul className="list-none m-0 p-0 flex flex-col gap-2 text-sm">
              {cleared.map((entry) => (
                <li key={entry.item.id}>
                  {entry.item.title}
                  <span className="block muted">{entry.outcomeLabel}</span>
                </li>
              ))}
            </ul>
          </section>
        )}
      </aside>
    </div>
  );
}

function DecisionCard({
  data,
  onCleared,
  onSkip,
}: {
  data: DetailData;
  onCleared: (outcomeLabel: string) => void;
  onSkip: () => void;
}) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const navigate = useNavigate();
  const blocker = data.blocker!;
  const response = useBlockerResponse(blocker, (outcome) => {
    const label =
      response.outcomes.find((entry) => entry.outcome === outcome)?.label ?? "";
    onCleared(label);
  });
  const responseRef = useRef(response);
  responseRef.current = response;

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

  return (
    <article
      className="ib-card enter overflow-hidden"
      aria-labelledby="decision-title"
    >
      <div className="p-5 desk:p-7 flex flex-col gap-4">
        <CoworkerLine
          coworker={data.item.presentation.coworker}
          text={localized(data.item.presentation.title, locale)}
        />
        <ItemProvenance data={data} />
        <h1 id="decision-title" className="ib-hero ib-hero--page">
          {localized(blocker.title, locale)}
        </h1>
        <p className="m-0 text-ink-2">
          {localized(blocker.description, locale)}
        </p>
        <p className="m-0 text-sm muted">
          {data.item.title} · {data.item.source.title}
        </p>
        <ExactContent data={data} />
        {response.fields}
        {hasPresentedResult(producingNode(data)) && (
          <Irreversible>
            {t("turn.exactNote", {
              name: data.item.presentation.coworker.name,
            })}
          </Irreversible>
        )}
      </div>
      <footer className="decision-footer">
        {response.error && (
          <p role="alert" className="form-error m-0">
            {response.error}
          </p>
        )}
        <OutcomeButtons
          response={response}
          keys
          extra={
            <Button
              full
              onClick={() =>
                void navigate({
                  to: "/work/$itemId",
                  params: { itemId: data.item.id },
                })
              }
            >
              {t("turn.openItem")}
              <Kbd>{t("key.e", { ns: "app" })}</Kbd>
            </Button>
          }
        />
        <button type="button" className="text-link self-start" onClick={onSkip}>
          {t("turn.later")}
        </button>
      </footer>
    </article>
  );
}

function AllClear({
  cleared,
  startedAt,
  skipped,
}: {
  cleared: Cleared[];
  startedAt: Date;
  skipped: number;
}) {
  const { t } = useTranslation("work");
  const locale = useLocale();
  const session = useSession();
  const waiting = useLiveQuery(() => listWorkItems({ state: "waiting" }), []);
  const next =
    waiting.status === "ready" ? nextDeadline(waiting.data) : undefined;
  const seconds = Math.round((Date.now() - startedAt.getTime()) / 1000);
  const me = session.profile.displayName || session.profile.email;
  return (
    <Focus>
      <div className="max-w-[640px] mx-auto text-center flex flex-col items-center gap-5 py-6">
        <div className="pile ib-pile" aria-hidden="true">
          <span />
          <span />
          <span />
          <span className="pile__check">
            <CheckIcon />
          </span>
        </div>
        <h1 className="ib-hero">{t("allClear.title")}</h1>
        <p className="lead m-0">
          {skipped > 0
            ? t("allClear.skipped", { count: skipped })
            : next
              ? t("allClear.next", { at: formatTime(next, locale) })
              : t("allClear.nothingNext")}
        </p>
        {cleared.length > 0 && (
          <ul className="ib-card ib-divide list-none m-0 p-0 w-full text-left">
            {cleared.map((entry) => (
              <li
                key={entry.item.id}
                className="px-4 py-3 flex items-center gap-3"
              >
                <span className="ib-num muted text-sm">
                  {formatTime(entry.at, locale)}
                </span>
                <CoworkerMark
                  coworker={entry.item.presentation.coworker}
                  size={24}
                  state="done"
                  decorative
                />
                <span className="min-w-0 flex-1">
                  <strong className="block">{entry.item.title}</strong>
                  <span className="block muted text-sm">
                    {entry.outcomeLabel}
                  </span>
                </span>
                <span className="text-sm text-ok-ink">
                  {t("allClear.by", { name: me })}
                </span>
              </li>
            ))}
          </ul>
        )}
        {cleared.length > 0 && (
          <p className="m-0 muted text-sm ib-num">
            {t("allClear.summary", {
              count: cleared.length,
              minutes: Math.floor(seconds / 60),
              seconds: seconds % 60,
            })}
          </p>
        )}
        <div className="flex flex-wrap justify-center gap-3">
          <Button asChild variant="ink">
            <Link to="/">{t("allClear.backToToday")}</Link>
          </Button>
          <Button asChild>
            <Link to="/work" search={{ period: "today" }}>
              {t("allClear.seeAllWork")}
            </Link>
          </Button>
        </div>
      </div>
    </Focus>
  );
}
