import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { forwardRef, type ButtonHTMLAttributes } from "react";
import { cn } from "../../lib/cn";

// shadcn/ui Button on the Design System's .ib-btn component (README §5):
// primary (blue), ink, secondary, ghost and danger, in sm/md/lg.
export const buttonVariants = cva("ib-btn", {
  variants: {
    variant: {
      primary: "ib-btn--primary",
      ink: "ib-btn--ink",
      secondary: "ib-btn--secondary",
      ghost: "ib-btn--ghost",
      danger: "ib-btn--danger",
    },
    size: {
      sm: "ib-btn--sm",
      md: "ib-btn--md",
      lg: "ib-btn--lg",
    },
    full: { true: "ib-btn--full" },
  },
  defaultVariants: { variant: "secondary", size: "md" },
});

export interface ButtonProps
  extends
    ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  asChild?: boolean;
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  ({ className, variant, size, full, asChild = false, ...props }, ref) => {
    const Component = asChild ? Slot : "button";
    return (
      <Component
        ref={ref}
        className={cn(buttonVariants({ variant, size, full }), className)}
        {...(asChild ? {} : { type: props.type ?? "button" })}
        {...props}
      />
    );
  },
);
Button.displayName = "Button";

/** A keyboard shortcut hint; always mirrored by the visible button it sits in. */
export function Kbd({ children }: { children: string }) {
  return (
    <kbd className="ib-kbd" aria-hidden="true">
      {children}
    </kbd>
  );
}
