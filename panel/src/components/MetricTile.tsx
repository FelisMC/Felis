import type { ReactNode } from "react";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";

/** MetricTile is one figure in a usage card: a labelled value with its chart or meter underneath. */
export function MetricTile({ icon: Icon, label, value, muted, tone, className, children }: {
  icon: LucideIcon;
  label: string;
  value: ReactNode;
  muted?: boolean;
  tone?: "warn" | "danger";
  className?: string;
  children?: ReactNode;
}) {
  return (
    <div className={cn("flex min-w-0 flex-col gap-2 p-3.5", className)}>
      <div className="flex items-center justify-between gap-1.5 text-muted-foreground">
        <span className="text-[11px] font-medium">{label}</span>
        <Icon
          className={cn(
            "h-3.5 w-3.5 shrink-0",
            tone === "danger" ? "text-destructive" : tone === "warn" ? "text-amber-500" : "text-muted-foreground/70",
          )}
          aria-hidden="true"
        />
      </div>
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
      {children && <div className="mt-auto">{children}</div>}
    </div>
  );
}
