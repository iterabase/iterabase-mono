import { useTranslation } from "react-i18next";
import type { ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { Button } from "./ui/button";

// Loading, empty, error and denial states. None of them shows numbers or
// progress the server has not returned.

export function Loading({ label }: { label?: string }) {
  const { t } = useTranslation("app");
  return (
    <div role="status" aria-live="polite" className="py-10 text-center muted">
      {label ?? t("loading")}
    </div>
  );
}

export function ErrorState({
  title,
  onRetry,
}: {
  title?: string;
  onRetry?: () => void;
}) {
  const { t } = useTranslation("app");
  return (
    <div
      role="alert"
      className="ib-card ib-card--pad my-6 flex flex-col items-start gap-3"
    >
      <strong>{title ?? t("errorTitle")}</strong>
      <p className="m-0 muted">{t("errorBody")}</p>
      {onRetry && (
        <Button size="sm" onClick={onRetry}>
          {t("retry")}
        </Button>
      )}
    </div>
  );
}

/** Shown for a screen the person's role cannot open (direct URLs included). */
export function Denied() {
  const { t } = useTranslation("app");
  return (
    <div
      role="alert"
      className="ib-card ib-card--pad my-6 flex flex-col items-start gap-3"
    >
      <h1 className="ib-hero ib-hero--page">{t("deniedTitle")}</h1>
      <p className="m-0 muted">{t("deniedBody")}</p>
      <Link to="/" className="text-link">
        {t("backToToday")}
      </Link>
    </div>
  );
}

export function NotFound() {
  const { t } = useTranslation("app");
  return (
    <div className="ib-card ib-card--pad my-6 flex flex-col items-start gap-3">
      <h1 className="ib-hero ib-hero--page">{t("notFoundTitle")}</h1>
      <p className="m-0 muted">{t("notFoundBody")}</p>
      <Link to="/" className="text-link">
        {t("backToToday")}
      </Link>
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="m-0 py-3 px-1 muted text-sm">{children}</p>;
}

/** A refresh failed: the screen keeps what it showed, and says so. */
export function StaleNotice({ onRetry }: { onRetry: () => void }) {
  const { t } = useTranslation("app");
  return (
    <div
      role="alert"
      className="form-error mb-4 flex items-center justify-between gap-3"
    >
      <span>{t("staleBody")}</span>
      <button type="button" className="text-link" onClick={onRetry}>
        {t("retry")}
      </button>
    </div>
  );
}
