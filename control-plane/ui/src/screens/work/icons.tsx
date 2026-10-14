import type { ReactNode } from "react";

// Line icons drawn on the Work and work item boards (24px grid, round caps).
// Always decorative: the text next to them carries the meaning.

type IconProps = { size?: number; stroke?: string; width?: number };

function Icon({
  size = 15,
  stroke = "currentColor",
  width = 2,
  children,
}: IconProps & { children: ReactNode }) {
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

export const ShieldIcon = (props: IconProps) => (
  <Icon size={13} stroke="var(--ink3)" {...props}>
    <path d="M12 3l7 3v6c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z" />
  </Icon>
);

export const ChevronRight = (props: IconProps) => (
  <Icon size={15} stroke="var(--ink3)" {...props}>
    <path d="M9 6l6 6-6 6" />
  </Icon>
);

export const ChevronLeft = (props: IconProps) => (
  <Icon size={18} stroke="var(--blue)" width={2.2} {...props}>
    <path d="M15 18l-6-6 6-6" />
  </Icon>
);

export const WarnIcon = (props: IconProps) => (
  <Icon size={16} stroke="var(--crit)" {...props}>
    <path d="M12 4l9 16H3z" />
    <path d="M12 10v4M12 17v.5" />
  </Icon>
);

export const CheckIcon = (props: IconProps) => (
  <Icon size={15} stroke="var(--ok)" width={2.6} {...props}>
    <path d="M5 12l5 5 9-10" />
  </Icon>
);

export const DocIcon = (props: IconProps) => (
  <Icon size={15} stroke="var(--ink3)" {...props}>
    <path d="M7 3h7l5 5v13H7z" />
    <path d="M14 3v5h5" />
  </Icon>
);

export const DotsIcon = (props: IconProps) => (
  <Icon size={18} {...props}>
    <circle cx="5" cy="12" r="1.4" />
    <circle cx="12" cy="12" r="1.4" />
    <circle cx="19" cy="12" r="1.4" />
  </Icon>
);

export const CloseIcon = (props: IconProps) => (
  <Icon size={17} {...props}>
    <path d="M6 6l12 12M18 6 6 18" />
  </Icon>
);
