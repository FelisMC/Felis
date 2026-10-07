import * as React from "react";
import { Check } from "lucide-react";
import { cn } from "@/lib/utils";

export const Checkbox = React.forwardRef<
  HTMLInputElement,
  Omit<React.InputHTMLAttributes<HTMLInputElement>, "type">
>(({ className, ...props }, ref) => (
  <span className={cn("relative inline-flex h-4 w-4 shrink-0", className)}>
    <input
      ref={ref}
      type="checkbox"
      className="peer h-4 w-4 cursor-pointer appearance-none rounded border border-input bg-transparent transition-colors checked:border-primary checked:bg-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background disabled:cursor-not-allowed disabled:opacity-50"
      {...props}
    />
    <Check aria-hidden="true" className="pointer-events-none absolute inset-0 h-4 w-4 text-primary-foreground opacity-0 peer-checked:opacity-100 peer-checked:peer-disabled:opacity-50" />
  </span>
));
Checkbox.displayName = "Checkbox";
