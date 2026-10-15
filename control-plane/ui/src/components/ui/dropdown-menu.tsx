import * as MenuPrimitive from "@radix-ui/react-dropdown-menu";
import {
  forwardRef,
  type ComponentPropsWithoutRef,
  type ElementRef,
} from "react";
import { cn } from "../../lib/cn";

// shadcn/ui DropdownMenu on Radix (avatar menu, ⋯ item menu).
export const DropdownMenu = MenuPrimitive.Root;
export const DropdownMenuTrigger = MenuPrimitive.Trigger;

export const DropdownMenuContent = forwardRef<
  ElementRef<typeof MenuPrimitive.Content>,
  ComponentPropsWithoutRef<typeof MenuPrimitive.Content>
>(({ className, sideOffset = 6, ...props }, ref) => (
  <MenuPrimitive.Portal>
    <MenuPrimitive.Content
      ref={ref}
      sideOffset={sideOffset}
      className={cn("menu", className)}
      {...props}
    />
  </MenuPrimitive.Portal>
));
DropdownMenuContent.displayName = "DropdownMenuContent";

export const DropdownMenuItem = forwardRef<
  ElementRef<typeof MenuPrimitive.Item>,
  ComponentPropsWithoutRef<typeof MenuPrimitive.Item>
>(({ className, ...props }, ref) => (
  <MenuPrimitive.Item
    ref={ref}
    className={cn("menu__item", className)}
    {...props}
  />
));
DropdownMenuItem.displayName = "DropdownMenuItem";

export function DropdownMenuSeparator() {
  return <MenuPrimitive.Separator className="menu__sep" />;
}

export function DropdownMenuLabel({ children }: { children: React.ReactNode }) {
  return (
    <MenuPrimitive.Label className="menu__label">
      {children}
    </MenuPrimitive.Label>
  );
}
