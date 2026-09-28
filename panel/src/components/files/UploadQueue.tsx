import { useTranslation } from "react-i18next";
import { AlertCircle, AlertTriangle, CheckCircle2, Clock, Loader2, RotateCw, Upload, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { formatBytes } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { UploadItem } from "./useUploads";

interface Props {
  items: readonly UploadItem[];
  /** The folder on screen: an upload headed elsewhere names its folder. */
  dir: string;
  onReplace: (id: number) => void;
  onRetry: (id: number) => void;
  onRemove: (id: number) => void;
  onReplaceAll: () => void;
  onSkipAll: () => void;
  onClearDone: () => void;
}

// UploadQueue shows each upload and what it waits on. A file already there asks
// replace or skip on its own row, so a batch never overwrites anything unasked.
export function UploadQueue({ items, dir, onReplace, onRetry, onRemove, onReplaceAll, onSkipAll, onClearDone }: Props) {
  const { t } = useTranslation("files");
  if (items.length === 0) return null;
  const done = items.filter((it) => it.state === "done").length;
  const asking = items.filter((it) => it.state === "exists").length;

  return (
    <section aria-label={t("uploads_label")} className="border-b border-border bg-muted/20">
      <div className="flex flex-wrap items-center gap-2 px-4 pt-3 pb-2">
        <Upload className="h-4 w-4 text-muted-foreground" />
        <p className="text-sm font-medium">{t("uploads_title", { done, total: items.length })}</p>
        <div className="ml-auto flex flex-wrap items-center gap-1">
          {asking > 1 && (
            <>
              <Button size="sm" variant="outline" className="h-7 text-xs" onClick={onReplaceAll}>
                {t("upload_replace_all")}
              </Button>
              <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={onSkipAll}>
                {t("upload_skip_all")}
              </Button>
            </>
          )}
          {done > 0 && (
            <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={onClearDone}>
              {t("upload_clear_done")}
            </Button>
          )}
        </div>
      </div>
      <ul className="max-h-72 divide-y divide-border/60 overflow-y-auto px-4 pb-2">
        {items.map((it) => (
          <UploadRow
            key={it.id}
            item={it}
            elsewhere={it.dir !== dir}
            onReplace={() => onReplace(it.id)}
            onRetry={() => onRetry(it.id)}
            onRemove={() => onRemove(it.id)}
          />
        ))}
      </ul>
    </section>
  );
}

function UploadRow({
  item,
  elsewhere,
  onReplace,
  onRetry,
  onRemove,
}: {
  item: UploadItem;
  elsewhere: boolean;
  onReplace: () => void;
  onRetry: () => void;
  onRemove: () => void;
}) {
  const { t } = useTranslation("files");
  const { file, state } = item;
  // The body is all sent and the Job is landing it. A file sent in one request
  // lands in seconds, with no bytes left to count; one sent in parts is fetched
  // by its Job afresh, which reports how far it has got.
  const landing = state === "uploading" && item.sent >= file.size;
  const written = landing && item.landing !== null && item.landing.total > 0 ? item.landing : null;
  const percent = written
    ? Math.min(100, Math.floor((written.done * 100) / written.total))
    : file.size > 0
      ? Math.min(100, Math.floor((item.sent * 100) / file.size))
      : 100;

  const icon = {
    queued: <Clock className="h-4 w-4 text-muted-foreground" />,
    uploading: <Loader2 className="h-4 w-4 animate-spin text-primary" />,
    done: <CheckCircle2 className="h-4 w-4 text-emerald-500" />,
    exists: <AlertTriangle className="h-4 w-4 text-amber-500" />,
    failed: <AlertCircle className="h-4 w-4 text-destructive" />,
  }[state];

  let status: string;
  if (state === "queued") status = t("upload_queued");
  else if (written) status = t("upload_landing_progress", { percent });
  else if (landing) status = t("upload_landing");
  else if (state === "uploading") status = t("upload_sending", { sent: formatBytes(item.sent), total: formatBytes(file.size), percent });
  else if (state === "done") status = t("upload_done");
  else if (state === "exists") status = t("upload_exists");
  else status = item.error ?? "";

  return (
    <li className="py-2">
      <div className="flex items-start gap-2.5">
        <span className="mt-0.5 shrink-0">{icon}</span>
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-baseline gap-2">
            <span className="truncate font-mono text-xs" title={file.name}>
              {file.name}
            </span>
            <span className="shrink-0 text-[11px] text-muted-foreground tabular-nums">{formatBytes(file.size)}</span>
          </div>
          {elsewhere && (
            <p className="truncate font-mono text-[11px] text-muted-foreground">
              → {item.dir === "" ? t("root") : `${item.dir}/`}
            </p>
          )}
          <p
            className={cn(
              "mt-0.5 text-xs tabular-nums",
              state === "failed"
                ? "text-destructive"
                : state === "exists"
                  ? "text-amber-600 dark:text-amber-400"
                  : "text-muted-foreground",
            )}
          >
            {status}
          </p>
          {state === "uploading" && (
            <div
              role="progressbar"
              aria-label={t("upload_progress_label", { name: file.name })}
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={percent}
              className="mt-1.5 h-1 w-full overflow-hidden rounded-full bg-muted"
            >
              <div
                className={cn(
                  "h-full rounded-full bg-primary transition-[width] duration-300 ease-out",
                  landing && !written && "animate-pulse",
                )}
                style={{ width: `${percent}%` }}
              />
            </div>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-1">
          {state === "exists" && (
            <>
              <Button size="sm" variant="outline" className="h-7 text-xs" onClick={onReplace}>
                {t("upload_replace")}
              </Button>
              <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={onRemove}>
                {t("upload_skip")}
              </Button>
            </>
          )}
          {state === "failed" && item.retryable && (
            <Button size="sm" variant="outline" className="h-7 text-xs" onClick={onRetry}>
              <RotateCw className="h-3.5 w-3.5" />
              {t("upload_retry")}
            </Button>
          )}
          {/* Once the body is all sent the Job may already be landing it, and
              a cancel could no longer say whether the file changed. */}
          {state !== "exists" && state !== "done" && !landing && (
            <Button
              size="sm"
              variant="ghost"
              className="h-7 w-7 p-0"
              onClick={onRemove}
              aria-label={t(state === "failed" ? "upload_dismiss" : "upload_cancel", { name: file.name })}
              title={t(state === "failed" ? "upload_dismiss" : "upload_cancel", { name: file.name })}
            >
              <X className="h-3.5 w-3.5" />
            </Button>
          )}
        </div>
      </div>
    </li>
  );
}
