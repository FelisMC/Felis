import type { ReactNode } from "react";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";

/** MetricTile is one figure in a usage card: a labelled value with its chart or meter underneath. */
export function MetricTile({ label, value, muted, tone, className, children }: {
  icon?: LucideIcon;
  label: string;
  value: ReactNode;
  muted?: boolean;
  tone?: "warn" | "danger";
  className?: string;
  children?: ReactNode;
}) {
  return (
    <div className={cn("flex min-w-0 flex-col gap-1.5 p-3.5", className)}>
      <span className="text-[11px] text-muted-foreground">{label}</span>
      <div
        className={cn(
          "break-words font-mono font-semibold leading-tight tracking-tight tabular-nums",
          muted ? "text-xs font-normal text-muted-foreground" : "text-[15px] text-foreground",
          tone === "warn" && "text-amber-600 dark:text-amber-400",
          tone === "danger" && "text-destructive",
        )}
      >
        {value}
      </div>
      {children && <div className="mt-auto pt-1">{children}</div>}
    </div>
  );
}
