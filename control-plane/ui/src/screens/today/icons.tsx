// Line icons drawn on the Today, Your turn and All clear boards (24px grid,
// round caps). Always decorative: the text next to them carries the meaning.

type IconProps = { size?: number; stroke?: string; width?: number };

function Icon({
  size = 15,
  stroke = "currentColor",
  width = 2,
  children,
}: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke={stroke}
      strokeWidth={width}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      className="flex-none"
    >
      {children}
    </svg>
  );
}

export const LockIcon = (props: IconProps) => (
  <Icon size={13} stroke="var(--ink3)" {...props}>
    <rect x="5" y="11" width="14" height="9" rx="2" />
    <path d="M8 11V8a4 4 0 0 1 8 0v3" />
  </Icon>
);

export const ChevronIcon = (props: IconProps) => (
  <Icon size={15} stroke="var(--ink3)" {...props}>
    <path d="M9 6l6 6-6 6" />
  </Icon>
);

export const ClockIcon = (props: IconProps) => (
  <Icon size={18} stroke="var(--sun)" {...props}>
    <circle cx="12" cy="12" r="9" />
    <path d="M12 7v5l3 2" />
  </Icon>
);

export const ArrowIcon = (props: IconProps) => (
  <Icon size={18} width={2.4} {...props}>
    <path d="M5 12h14M13 6l6 6-6 6" />
  </Icon>
);

export const CloseIcon = (props: IconProps) => (
  <Icon size={16} {...props}>
    <path d="M6 6l12 12M18 6 6 18" />
  </Icon>
);

export const InfoIcon = (props: IconProps) => (
  <Icon size={17} {...props}>
    <circle cx="12" cy="12" r="9" />
    <path d="M12 11v5M12 8v.5" />
  </Icon>
);

export const CheckIcon = (props: IconProps) => (
  <Icon size={15} stroke="var(--ok)" width={2.6} {...props}>
    <path d="M5 12l5 5 9-10" />
  </Icon>
);

export const SearchIcon = (props: IconProps) => (
  <Icon size={18} {...props}>
    <circle cx="11" cy="11" r="7" />
    <path d="m20 20-3.5-3.5" />
  </Icon>
);

/** The green check in a soft circle that marks done rows. */
export function DoneCheck({ size = 28 }: { size?: 22 | 24 | 26 | 28 }) {
  return (
    <span className={`done-check done-check--${size}`} aria-hidden="true">
      <CheckIcon size={Math.round(size / 2)} stroke="var(--ok-ink)" width={3} />
    </span>
  );
}
