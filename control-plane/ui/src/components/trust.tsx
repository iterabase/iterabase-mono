import { useTranslation } from "react-i18next";
import type { ReactNode } from "react";
import { InfoIcon, TrustIcon } from "./brand";

// The trust layer (DS README §7): the standing trust line, provenance, the
// exact content block and the irreversibility note.

export function TrustLine() {
  const { t } = useTranslation("app");
  return (
    <p className="ib-trust m-0">
      <TrustIcon />
      {t("trustLine")}
    </p>
  );
}

/** "Prepared by Quotes · 09:31 · checked 3 supplier replies". */
export function Provenance({ children }: { children: ReactNode }) {
  return <p className="ib-provenance m-0">{children}</p>;
}

/** The literal content a decision is about, shown exactly. */
export function Exact({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <section className="ib-exact" aria-label={label}>
      {children}
    </section>
  );
}

export function Irreversible({ children }: { children: ReactNode }) {
  return (
    <p className="ib-irreversible m-0">
      <InfoIcon />
      {children}
    </p>
  );
}
