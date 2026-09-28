import { useTranslation } from "react-i18next";
import { AlertCircle, CheckCircle2, FileArchive, Loader2, Upload, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { MessageLine } from "@/components/MessageLine";
import { humanizeError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import type { FileOp } from "@/lib/types";
import { cn } from "@/lib/utils";
import { opErrorText } from "./opText";

interface Props {
  ops: readonly FileOp[];
  /** Why the latest read of the ops failed, if it did. */
  error: unknown;
  onDismiss: (id: string) => void;
  /** Opens the list of files an extraction stopped short of replacing. */
  onConflicts: (op: FileOp) => void;
}

// FileOps shows the server's background operations: how far a running one has
// got, and how each recent one ended, until it is dismissed.
export function FileOps({ ops, error, onDismiss, onConflicts }: Props) {
  const { t } = useTranslation("files");
  if (ops.length === 0 && error == null) return null;

  return (
    <section aria-label={t("ops_label")} className="border-b border-border bg-muted/20">
      {error != null && (
        <div className="px-4 pt-3">
          <MessageLine kind="error" message={t("ops_refresh_failed", { reason: humanizeError(error) })} compact />
        </div>
      )}
      <ul className="divide-y divide-border/60 px-4 py-1">
        {ops.map((op) => (
          <OpRow key={op.id} op={op} onDismiss={() => onDismiss(op.id)} onConflicts={() => onConflicts(op)} />
        ))}
      </ul>
    </section>
  );
}

function OpRow({ op, onDismiss, onConflicts }: { op: FileOp; onDismiss: () => void; onConflicts: () => void }) {
  const { t } = useTranslation("files");
  const title = t(op.op === "unzip" ? "op_unzip" : "op_upload", { path: op.path });
  const Kind = op.op === "unzip" ? FileArchive : Upload;
  const counted = op.total > 0;
  const percent = counted ? Math.min(100, Math.floor((op.done * 100) / op.total)) : 0;
  const failure = op.error ?? { code: "job_failed", message: "" };
  const conflicts = op.state === "failed" && op.op === "unzip" && failure.code === "file_exists";

  let status: string;
  if (op.state === "running") {
    status = counted
      ? t("op_progress", { done: formatBytes(op.done), total: formatBytes(op.total), percent })
      : t("op_preparing");
  } else if (op.state === "succeeded") {
    status =
      op.op === "unzip"
        ? t("op_unzip_done", { count: op.files ?? 0, bytes: formatBytes(op.bytes ?? 0) })
        : t("op_upload_done");
  } else {
    status = opErrorText(op.op, failure);
  }

  const icon =
    op.state === "running" ? (
      <Loader2 className="h-4 w-4 animate-spin text-primary" />
    ) : op.state === "succeeded" ? (
      <CheckCircle2 className="h-4 w-4 text-emerald-500" />
    ) : (
      <AlertCircle className="h-4 w-4 text-destructive" />
    );

  return (
    <li className="py-2">
      <div className="flex items-start gap-2.5">
        <span className="mt-0.5 shrink-0">{icon}</span>
        <div className="min-w-0 flex-1">
          <p className="flex min-w-0 items-center gap-1.5">
            <Kind className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
            <span className="truncate font-mono text-xs" title={op.path}>
              {title}
            </span>
          </p>
          <p
            className={cn(
              "mt-0.5 text-xs tabular-nums",
              op.state === "failed" ? "text-destructive" : "text-muted-foreground",
            )}
          >
            {status}
          </p>
          {op.state === "running" && (
            <div
              role="progressbar"
              aria-label={t("op_progress_label", { path: op.path })}
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={counted ? percent : undefined}
              className="mt-1.5 h-1 w-full overflow-hidden rounded-full bg-muted"
            >
              <div
                className={cn(
                  "h-full rounded-full bg-primary transition-[width] duration-300 ease-out",
                  !counted && "w-full animate-pulse",
                )}
                style={counted ? { width: `${percent}%` } : undefined}
              />
            </div>
          )}
        </div>
        {op.state !== "running" && (
          <div className="flex shrink-0 items-center gap-1">
            {conflicts && (
              <Button size="sm" variant="outline" className="h-7 text-xs" onClick={onConflicts}>
                {t("op_conflicts")}
              </Button>
            )}
            <Button
              size="sm"
              variant="ghost"
              className="h-7 w-7 p-0"
              onClick={onDismiss}
              aria-label={t("op_dismiss", { path: op.path })}
              title={t("op_dismiss", { path: op.path })}
            >
              <X className="h-3.5 w-3.5" />
            </Button>
          </div>
        )}
      </div>
    </li>
  );
}
