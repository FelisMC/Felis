import type { ReactNode } from "react";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";

/** MetricTile is one figure in a usage card: a labelled value with its chart or meter underneath. */
export function MetricTile({ icon: Icon, label, value, muted, tone, children }: {
  icon: LucideIcon;
  label: string;
  value: ReactNode;
  /** muted greys out a value that stands in for missing data. */
  muted?: boolean;
  tone?: "warn" | "danger";
  children?: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-3 rounded-lg border border-border/60 bg-muted/10 p-4">
      <div className="flex items-center gap-1.5 text-muted-foreground">
        <Icon className={cn("h-3.5 w-3.5", tone === "danger" ? "text-destructive" : tone === "warn" ? "text-amber-500" : "text-primary")} aria-hidden="true" />
        <span className="text-[11px] font-medium">{label}</span>
      </div>
      <div
        className={cn(
          "break-words font-mono font-bold leading-tight tabular-nums",
          muted ? "text-sm text-muted-foreground" : "text-xl text-foreground",
          tone === "warn" && "text-amber-600 dark:text-amber-400",
          tone === "danger" && "text-destructive",
        )}
      >
        {value}
      </div>
      {children && <div className="mt-auto">{children}</div>}
    </div>
  );
}
