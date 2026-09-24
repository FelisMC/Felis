import { useState } from "react";
import { AlertTriangle, Check, CheckCircle2, Copy, Database, Loader2, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { formatAbsolute, formatBytes, formatRelative } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { DBBackupLabel } from "@/lib/types";

// The control-plane database backup is taken on the host (felis-db-backup.timer),
// never through the API, so the card is read-only: it says how fresh the newest
// backup is and, when it is not, hands the admin the exact host commands.

const LABEL_KEY: Record<DBBackupLabel, string> = {
  daily: "dbbackup_label_daily",
  "pre-migrate": "dbbackup_label_pre_migrate",
  "pre-restore": "dbbackup_label_pre_restore",
  manual: "dbbackup_label_manual",
};

const FIX_COMMANDS = ["sudo felis db backup", "journalctl -u felis-db-backup -n 50 --no-pager"];

function CopyCommand({ command }: { command: string }) {
  const { t } = useTranslation("admin");
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard denied (non-secure context): the command stays selectable.
    }
  }
  return (
    <div className="flex items-center gap-2 rounded-md border border-border/60 bg-muted/40 pl-3 pr-1 py-1">
      <code className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap py-1 font-mono text-xs text-foreground" title={command}>
        {command}
      </code>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="h-7 w-7 shrink-0"
        onClick={copy}
        aria-label={t("dbbackup_copy")}
        title={t("dbbackup_copy")}
      >
        {copied ? <Check className="h-3.5 w-3.5 text-emerald-500" /> : <Copy className="h-3.5 w-3.5" />}
      </Button>
    </div>
  );
}

function Field({ label, children, title }: { label: string; children: React.ReactNode; title?: string }) {
  return (
    <div className="min-w-0 space-y-1">
      <dt className="text-[11px] font-medium text-muted-foreground">{label}</dt>
      <dd className="truncate text-sm font-semibold text-foreground" title={title}>
        {children}
      </dd>
    </div>
  );
}

export function DBBackupCard() {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const { data, error, loading, reload } = useAsync(() => api.getDBBackup(), []);
  const last = data?.last ?? null;
  const maxAgeHours = data ? Math.round(data.max_age_seconds / 3600) : 26;

  const state: "loading" | "error" | "never" | "stale" | "ok" = !data
    ? error
      ? "error"
      : "loading"
    : !last
      ? "never"
      : data.stale
        ? "stale"
        : "ok";

  const badge = (() => {
    switch (state) {
      case "ok":
        return (
          <Badge className="gap-1 border-transparent bg-emerald-500/15 text-emerald-500">
            <CheckCircle2 className="h-3 w-3" />
            {t("dbbackup_status_ok")}
          </Badge>
        );
      case "stale":
        return (
          <Badge variant="destructive" className="gap-1">
            <AlertTriangle className="h-3 w-3" />
            {t("dbbackup_status_stale")}
          </Badge>
        );
      case "never":
        return (
          <Badge variant="destructive" className="gap-1">
            <AlertTriangle className="h-3 w-3" />
            {t("dbbackup_status_never")}
          </Badge>
        );
      default:
        return null;
    }
  })();

  return (
    <Card className="w-full">
      <CardHeader className="flex flex-row items-center justify-between gap-3 space-y-0">
        <div className="flex min-w-0 items-center gap-3">
          <div
            className={cn(
              "hidden rounded-md p-2 sm:block",
              state === "stale" || state === "never"
                ? "bg-destructive/10 text-destructive"
                : "bg-primary/10 text-primary",
            )}
          >
            <Database className="h-5 w-5" />
          </div>
          <div className="min-w-0">
            <CardTitle className="flex flex-wrap items-center gap-2 text-base font-semibold">
              {t("dbbackup_title")}
              {badge}
            </CardTitle>
            <p className="mt-0.5 text-xs text-muted-foreground">{t("dbbackup_subtitle")}</p>
          </div>
        </div>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="h-8 w-8 shrink-0"
          onClick={reload}
          disabled={loading}
          aria-label={t("dbbackup_refresh")}
          title={t("dbbackup_refresh")}
        >
          <RefreshCw className={cn("h-4 w-4", loading && "animate-spin")} />
        </Button>
      </CardHeader>

      <CardContent className="space-y-4 text-sm">
        {state === "loading" && (
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            {t("dbbackup_loading")}
          </div>
        )}

        {state === "error" && <MessageLine kind="error" message={humanizeError(error)} />}

        {last && (
          <dl className="grid grid-cols-2 gap-x-6 gap-y-4 lg:grid-cols-4">
            <Field label={t("dbbackup_field_when")} title={formatAbsolute(last.at, locale)}>
              <span className={cn(state === "stale" && "text-destructive")}>
                {formatRelative(last.at, Date.now(), locale) || "—"}
              </span>
              <span className="block truncate text-[11px] font-normal text-muted-foreground">
                {formatAbsolute(last.at, locale)}
              </span>
            </Field>
            <Field label={t("dbbackup_field_label")}>
              {LABEL_KEY[last.label] ? t(LABEL_KEY[last.label]) : last.label}
            </Field>
            <Field label={t("dbbackup_field_size")}>
              <span className="font-mono">{formatBytes(last.size_bytes)}</span>
            </Field>
            <Field label={t("dbbackup_field_schema")}>
              <span className="font-mono">{last.schema_version ? `#${last.schema_version}` : "—"}</span>
            </Field>
            <div className="col-span-2 min-w-0 space-y-1 lg:col-span-4">
              <dt className="text-[11px] font-medium text-muted-foreground">{t("dbbackup_field_file")}</dt>
              <dd className="break-all font-mono text-xs text-foreground">
                {last.dir.replace(/\/+$/, "")}/{last.name}
              </dd>
            </div>
          </dl>
        )}

        {(state === "stale" || state === "never") && (
          <div className="space-y-3 rounded-lg border border-destructive/25 bg-destructive/5 p-4">
            <div className="flex items-start gap-2 text-destructive">
              <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
              <div className="space-y-1">
                <p className="font-semibold">
                  {state === "never" ? t("dbbackup_never_title") : t("dbbackup_stale_title", { hours: maxAgeHours })}
                </p>
                <p className="text-xs leading-relaxed text-destructive/90">{t("dbbackup_fix_hint")}</p>
              </div>
            </div>
            <div className="space-y-2">
              {FIX_COMMANDS.map((c) => (
                <CopyCommand key={c} command={c} />
              ))}
            </div>
          </div>
        )}

        {data && (
          <p className="rounded-md border border-border/40 bg-muted/15 p-3 text-[11px] leading-relaxed text-muted-foreground">
            {t("dbbackup_offsite_note")}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
