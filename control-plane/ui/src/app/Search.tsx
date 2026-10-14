import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { CoworkerMark } from "../components/coworker";
import { StateBadge } from "../components/work-state";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from "../components/ui/dialog";
import { useLocale } from "../i18n";
import { listWorkItems } from "../lib/api";
import type { WorkItem } from "../lib/types";
import { localized } from "../work/model";
import { useLive } from "./live";
import { useSession } from "./session";

// ⌘K search across work items, AI coworkers and customers (customers are the
// people and companies in work sources, which the work-item search covers).
// Asking Sofia joins this dialog with HOR-452 (DES-HOR-459-03).

export type SearchTarget =
  | { to: "/work/$itemId"; params: { itemId: string } }
  | { to: "/team/$key"; params: { key: string } };

export function SearchDialog({
  open,
  onOpenChange,
  onPick,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onPick: (target: SearchTarget) => void;
}) {
  const { t } = useTranslation("app");
  const locale = useLocale();
  const session = useSession();
  const { workflows } = useLive();
  const [query, setQuery] = useState("");
  const [items, setItems] = useState<WorkItem[] | null>(null);
  const [failed, setFailed] = useState(false);
  const [active, setActive] = useState(0);

  useEffect(() => {
    if (!open) {
      setQuery("");
      setItems(null);
      setFailed(false);
    }
  }, [open]);

  useEffect(() => {
    const text = query.trim();
    if (!open || text.length < 2) {
      setItems(null);
      return;
    }
    let cancelled = false;
    const timer = setTimeout(() => {
      listWorkItems({ search: text, limit: 8 })
        .then((found) => {
          if (!cancelled) {
            setItems(found);
            setFailed(false);
          }
        })
        .catch((err: unknown) => {
          if (!cancelled && !session.lost(err)) setFailed(true);
        });
    }, 200);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [query, open, session]);

  const coworkers = useMemo(() => {
    const text = query.trim().toLowerCase();
    if (!text) return [];
    return (workflows ?? []).filter((workflow) =>
      [
        workflow.coworker.name,
        localized(workflow.coworker.role, locale),
        localized(workflow.title, locale),
      ]
        .join(" ")
        .toLowerCase()
        .includes(text),
    );
  }, [query, workflows, locale]);

  const results: Array<{ key: string; target: SearchTarget }> = [
    ...coworkers.map((workflow) => ({
      key: `cw-${workflow.key}`,
      target: {
        to: "/team/$key",
        params: { key: workflow.key },
      } as SearchTarget,
    })),
    ...(items ?? []).map((item) => ({
      key: `wi-${item.id}`,
      target: {
        to: "/work/$itemId",
        params: { itemId: item.id },
      } as SearchTarget,
    })),
  ];

  useEffect(() => setActive(0), [query, items]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent aria-describedby="search-help">
        <DialogTitle className="m-0 text-lg font-semibold">
          {t("search")}
        </DialogTitle>
        <DialogDescription id="search-help" className="mt-1 mb-3 muted text-sm">
          {t("searchHelp")}
        </DialogDescription>
        <input
          className="ib-input"
          type="search"
          autoFocus
          aria-label={t("search")}
          aria-controls="search-results"
          placeholder={t("searchInput")}
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "ArrowDown") {
              event.preventDefault();
              setActive((index) => Math.min(index + 1, results.length - 1));
            } else if (event.key === "ArrowUp") {
              event.preventDefault();
              setActive((index) => Math.max(index - 1, 0));
            } else if (event.key === "Enter" && results[active]) {
              event.preventDefault();
              onPick(results[active].target);
            }
          }}
        />
        {failed && (
          <p role="alert" className="form-error mt-3">
            {t("searchFailed")}
          </p>
        )}
        <ul
          id="search-results"
          className="mt-3 list-none p-0 m-0"
          aria-label={t("searchResults")}
        >
          {coworkers.map((workflow, index) => (
            <li key={workflow.key}>
              <button
                type="button"
                className="row-link rounded-lg"
                aria-current={active === index ? "true" : undefined}
                onClick={() =>
                  onPick({ to: "/team/$key", params: { key: workflow.key } })
                }
              >
                <CoworkerMark
                  coworker={workflow.coworker}
                  size={28}
                  decorative
                />
                <span className="min-w-0">
                  <strong>{workflow.coworker.name}</strong>
                  <span className="block muted text-sm">
                    {localized(workflow.coworker.role, locale)} ·{" "}
                    {t("aiCoworker")}
                  </span>
                </span>
              </button>
            </li>
          ))}
          {(items ?? []).map((item, index) => (
            <li key={item.id}>
              <button
                type="button"
                className="row-link rounded-lg"
                aria-current={
                  active === coworkers.length + index ? "true" : undefined
                }
                onClick={() =>
                  onPick({ to: "/work/$itemId", params: { itemId: item.id } })
                }
              >
                <CoworkerMark
                  coworker={item.presentation.coworker}
                  size={28}
                  decorative
                />
                <span className="min-w-0 flex-1">
                  <strong className="block truncate">{item.title}</strong>
                  <span className="block muted text-sm truncate">
                    {item.source.title}
                  </span>
                </span>
                <StateBadge item={item} />
              </button>
            </li>
          ))}
        </ul>
        {query.trim().length >= 2 && items !== null && results.length === 0 && (
          <p className="muted text-sm mt-3" role="status">
            {t("searchEmpty")}
          </p>
        )}
      </DialogContent>
    </Dialog>
  );
}
