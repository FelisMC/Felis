import * as React from "react";
import { cn } from "@/lib/utils";

interface SwitchProps extends Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, "onChange" | "onClick"> {
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
}

export const Switch = React.forwardRef<HTMLButtonElement, SwitchProps>(
  ({ checked, onCheckedChange, className, ...props }, ref) => (
    <button
      {...props}
      ref={ref}
      type="button"
      role="switch"
      aria-checked={checked}
      onClick={() => onCheckedChange(!checked)}
      className={cn(
        "inline-flex h-[18px] w-8 shrink-0 cursor-pointer items-center rounded-full p-0.5 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 focus-visible:ring-offset-1 focus-visible:ring-offset-background disabled:cursor-not-allowed disabled:opacity-45",
        checked ? "bg-primary" : "bg-muted-foreground/30 dark:bg-muted-foreground/25",
        className,
      )}
    >
      <span
        aria-hidden="true"
        className={cn(
          "h-3.5 w-3.5 rounded-full bg-white shadow-sm transition-transform motion-reduce:transition-none",
          checked ? "translate-x-3.5" : "translate-x-0",
        )}
      />
    </button>
  ),
);
Switch.displayName = "Switch";
