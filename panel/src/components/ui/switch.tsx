import * as React from "react";
import { cn } from "@/lib/utils";

interface SwitchProps extends Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, "onChange" | "onClick"> {
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
}

export const Switch = React.forwardRef<HTMLButtonElement, SwitchProps>(
  ({ checked, onCheckedChange, className, ...props }, ref) => (
    <button {...props} ref={ref} type="button" role="switch" aria-checked={checked} onClick={() => onCheckedChange(!checked)}
      className={cn("inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full p-0.5 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background disabled:cursor-not-allowed disabled:opacity-50", checked ? "bg-primary" : "bg-muted-foreground/25", className)}>
      <span aria-hidden="true" className={cn("h-4 w-4 rounded-full bg-white shadow-sm transition-transform motion-reduce:transition-none", checked ? "translate-x-4" : "translate-x-0")} />
    </button>
  ),
);
Switch.displayName = "Switch";
