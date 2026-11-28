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
      className="peer h-4 w-4 cursor-pointer appearance-none rounded-[4px] border border-input bg-background transition-[color,background-color,border-color,box-shadow,transform] duration-150 hover:border-muted-foreground/50 active:scale-90 checked:border-primary checked:bg-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 focus-visible:ring-offset-1 focus-visible:ring-offset-background disabled:cursor-not-allowed disabled:opacity-50"
      {...props}
    />
    <Check aria-hidden="true" className="pointer-events-none absolute inset-0 m-auto h-3 w-3 scale-50 stroke-[2.5] text-primary-foreground opacity-0 transition-[opacity,transform] duration-200 ease-[cubic-bezier(0.34,1.4,0.64,1)] peer-checked:scale-100 peer-checked:opacity-100 peer-checked:peer-disabled:opacity-50" />
  </span>
));
Checkbox.displayName = "Checkbox";
