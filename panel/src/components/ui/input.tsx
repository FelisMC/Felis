import * as React from "react";
import { cn } from "@/lib/utils";

export const Input = React.forwardRef<
  HTMLInputElement,
  React.InputHTMLAttributes<HTMLInputElement>
>(({ className, type, ...props }, ref) => (
  <input
    type={type}
    ref={ref}
    className={cn(
      "flex h-8 w-full rounded-md border border-input bg-card px-2.5 py-1 text-[12.5px] text-foreground transition-[border-color,box-shadow,background-color] duration-150 placeholder:text-muted-foreground/70 hover:border-muted-foreground/35 focus-visible:border-primary/70 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/20 disabled:cursor-not-allowed disabled:opacity-50 dark:bg-background",
      className,
    )}
    {...props}
  />
));
Input.displayName = "Input";
