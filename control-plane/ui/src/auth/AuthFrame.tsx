import type { ReactNode } from "react";
import { Logomark } from "../components/brand";
import { CoworkerMark } from "../components/coworker";
import type { CoworkerMark as Mark } from "../lib/types";
import { LockIcon, TickIcon } from "./icons";

// The signed-out page frame (XTSignIn / XTPhoneSignIn): logo, decorative
// coworker marks, the serif hero and the trust bullets on the left, the card on
// the right; one column on phone. The public page knows no coworkers, so the
// marks are the prototype's three generic shapes and colours.

const marks: Mark[] = [
  { shape: "round", color: "blue" },
  { shape: "square", color: "green" },
  { shape: "petal", color: "sun" },
];

export function Brand({ name }: { name: string }) {
  return (
    <span className="auth-brand">
      <Logomark size={30} />
      {name}
    </span>
  );
}

export function AuthFrame({
  brand,
  eyebrow,
  title,
  subline,
  trust,
  locale,
  children,
}: {
  brand: string;
  /** The installation's name above the hero (DES-HOR-459-08). */
  eyebrow?: string;
  title: string;
  subline: string;
  trust: [string, string, string];
  locale?: ReactNode;
  children: ReactNode;
}) {
  return (
    <main className="xt auth-page">
      <div className="auth-glow" aria-hidden="true" />
      {locale}
      <div className="auth-layout">
        <Brand name={brand} />
        <div className="auth-hero">
          <span className="auth-marks" aria-hidden="true">
            {marks.map((mark, index) => (
              <span key={mark.shape} className={index === 0 ? "breathe" : ""}>
                <CoworkerMark
                  coworker={{ name: "", mark }}
                  size={44}
                  decorative
                />
              </span>
            ))}
          </span>
          <div className="auth-hero-copy">
            {eyebrow && <p className="eyebrow m-0">{eyebrow}</p>}
            <h1 className="serif auth-title">{title}</h1>
            <p className="auth-sub">{subline}</p>
          </div>
        </div>
        <section className="auth-panel" aria-live="polite">
          <div className="auth-card">{children}</div>
        </section>
        <ul className="auth-trust">
          <li>
            <TickIcon />
            <span>{trust[0]}</span>
          </li>
          <li>
            <TickIcon />
            <span>{trust[1]}</span>
          </li>
          <li>
            <LockIcon />
            <span>{trust[2]}</span>
          </li>
        </ul>
      </div>
    </main>
  );
}

/** The probing and unavailable states: the logo and one centred card. */
export function AuthSolo({
  brand,
  children,
}: {
  brand: string;
  children: ReactNode;
}) {
  return (
    <main className="xt auth-page">
      <div className="auth-glow" aria-hidden="true" />
      <div className="auth-solo">
        <Brand name={brand} />
        <div className="auth-card">{children}</div>
      </div>
    </main>
  );
}
