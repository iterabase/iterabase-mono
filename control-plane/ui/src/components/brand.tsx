// Logo C, "Gathered" (the chosen 2026-10 lockup): four inputs on an arc, one
// warm, a blue output. The wordmark is text so it follows the type tokens.

export function Logomark({ size = 24 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 40 40"
      fill="none"
      aria-hidden="true"
      focusable="false"
    >
      <g stroke="#222F3E" strokeWidth="2" strokeLinecap="round" fill="none">
        <path d="M10 7 Q15 20 20 20" />
        <path d="M7 15 Q15 20 20 20" />
        <path d="M7 25 Q15 20 20 20" />
        <path d="M10 33 Q15 20 20 20" />
      </g>
      <circle cx="10" cy="7" r="3" fill="#222F3E" />
      <circle cx="7" cy="15" r="3" fill="#E39A52" />
      <circle cx="7" cy="25" r="3" fill="#222F3E" />
      <circle cx="10" cy="33" r="3" fill="#222F3E" />
      <circle
        cx="20"
        cy="20"
        r="5"
        fill="none"
        stroke="#0C1017"
        strokeWidth="2"
      />
      <circle cx="20" cy="20" r="2" fill="#0C1017" />
      <path
        d="M25 20 L31 20"
        stroke="#2D82F7"
        strokeWidth="2.6"
        strokeLinecap="round"
      />
      <circle cx="34" cy="20" r="3.4" fill="#2D82F7" />
    </svg>
  );
}

export function TrustIcon() {
  return (
    <svg viewBox="0 0 16 16" fill="none" aria-hidden="true" focusable="false">
      <rect
        x="3"
        y="7"
        width="10"
        height="7"
        rx="2"
        stroke="currentColor"
        strokeWidth="1.4"
      />
      <path
        d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2"
        stroke="currentColor"
        strokeWidth="1.4"
      />
    </svg>
  );
}

export function InfoIcon() {
  return (
    <svg
      viewBox="0 0 16 16"
      width="16"
      height="16"
      fill="none"
      aria-hidden="true"
      focusable="false"
    >
      <circle cx="8" cy="8" r="6.5" stroke="currentColor" strokeWidth="1.4" />
      <path
        d="M8 7v4M8 5h.01"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function CheckIcon({ size = 20 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 20 20"
      fill="none"
      aria-hidden="true"
      focusable="false"
    >
      <path
        d="M5 10.5l3.2 3L15 7"
        stroke="currentColor"
        strokeWidth="2.2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

export function CloseIcon() {
  return (
    <svg
      width="18"
      height="18"
      viewBox="0 0 18 18"
      fill="none"
      aria-hidden="true"
      focusable="false"
    >
      <path
        d="M4.5 4.5l9 9M13.5 4.5l-9 9"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function MoreIcon() {
  return (
    <svg
      width="18"
      height="18"
      viewBox="0 0 18 18"
      fill="currentColor"
      aria-hidden="true"
      focusable="false"
    >
      <circle cx="4" cy="9" r="1.5" />
      <circle cx="9" cy="9" r="1.5" />
      <circle cx="14" cy="9" r="1.5" />
    </svg>
  );
}

export function SearchIcon() {
  return (
    <svg
      width="16"
      height="16"
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden="true"
      focusable="false"
    >
      <circle cx="7" cy="7" r="4.5" stroke="currentColor" strokeWidth="1.5" />
      <path
        d="M10.5 10.5L14 14"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
      />
    </svg>
  );
}

/** Phone tab icons: Today (sun), Work (stack), Team (people), More (dots). */
export function TabIcon({
  name,
}: {
  name: "today" | "work" | "team" | "more";
}) {
  const common = {
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.7,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    "aria-hidden": true,
    focusable: false,
  };
  if (name === "today")
    return (
      <svg {...common}>
        <circle cx="12" cy="12" r="4" />
        <path d="M12 3v2M12 19v2M3 12h2M19 12h2M5.6 5.6l1.4 1.4M17 17l1.4 1.4M5.6 18.4L7 17M17 7l1.4-1.4" />
      </svg>
    );
  if (name === "work")
    return (
      <svg {...common}>
        <rect x="4" y="5" width="16" height="5" rx="1.5" />
        <rect x="4" y="14" width="16" height="5" rx="1.5" />
      </svg>
    );
  if (name === "team")
    return (
      <svg {...common}>
        <circle cx="9" cy="9" r="3" />
        <circle cx="17" cy="10" r="2.5" />
        <path d="M3.5 19c.8-3 3-4.5 5.5-4.5s4.7 1.5 5.5 4.5M15 15.2c2.3-.3 4.4.8 5.3 3.8" />
      </svg>
    );
  return (
    <svg {...common}>
      <circle cx="6" cy="12" r="1.2" />
      <circle cx="12" cy="12" r="1.2" />
      <circle cx="18" cy="12" r="1.2" />
    </svg>
  );
}
