import { AlertCircle, CheckCircle2 } from "lucide-react";
import { cn } from "@/lib/utils";

interface Props {
  kind: "error" | "success";
  message: string;
  /** Compact variant: smaller text, no border, no icon. Error-only. */
  compact?: boolean;
  className?: string;
}

const ICON = { error: AlertCircle, success: CheckCircle2 };

const STYLE = {
  error: "border-destructive/20 bg-destructive/10 text-destructive",
  success: "border-emerald-500/20 bg-emerald-500/10 text-emerald-500",
};

const COMPACT_STYLE = "bg-destructive/10 text-destructive";

export function MessageLine({ kind, message, compact, className }: Props) {
  if (compact) {
    return (
      <p
        className={cn(
          "text-xs font-medium rounded-md p-2.5",
          COMPACT_STYLE,
          className,
        )}
      >
        {message}
      </p>
    );
  }
  const Icon = ICON[kind];
  return (
    <div
      className={cn(
        "flex items-center gap-2 rounded-md border p-3 text-sm",
        STYLE[kind],
        className,
      )}
    >
      <Icon className="h-4 w-4 shrink-0" />
      <p>{message}</p>
    </div>
  );
}
