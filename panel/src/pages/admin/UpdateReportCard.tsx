import { AlertTriangle, ArrowUpCircle, CheckCircle2, Loader2, PackageCheck, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { MessageLine } from "@/components/MessageLine";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { formatAbsolute, formatRelative } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { UpdateComponent, UpdateComponentState } from "@/lib/types";
import { CopyCommand } from "./DBBackupCard";

// The installed versions are only readable on the host, so the check runs there
// (felis-update-check.timer → `felis update --record`) and this card reads what
// it recorded. Felis applies nothing on its own: an available update comes with
// the host command that prints how to apply it.

const STATE_STYLE: Record<UpdateComponentState, string> = {
  available: "bg-amber-500/10 text-amber-600 dark:text-amber-400 border-amber-500/25",
  current: "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 border-emerald-500/25",
  unknown: "bg-zinc-500/10 text-muted-foreground border-zinc-500/25",
  unreadable: "bg-rose-500/10 text-rose-500 border-rose-500/25",
  pinned: "bg-sky-500/10 text-sky-600 dark:text-sky-400 border-sky-500/25",
};

const FIX_COMMANDS = ["sudo felis update --record", "journalctl -u felis-update-check -n 50 --no-pager"];

const GRID = "sm:grid sm:grid-cols-[minmax(0,9rem)_minmax(0,1fr)_minmax(0,1fr)_7.5rem] sm:items-center sm:gap-3";

export function UpdateReportCard() {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const { data, error, loading, reload } = useAsync(() => api.getUpdateReport(), []);
  const report = data?.report ?? null;
  const maxAgeHours = data ? Math.round(data.max_age_seconds / 3600) : 26;
  const available = report?.components.filter((c) => c.state === "available") ?? [];
  const selectors = [...new Set(available.map((c) => c.selector).filter((s): s is string => !!s))];

  const state: "loading" | "error" | "never" | "stale" | "ok" = !data
    ? error
      ? "error"
      : "loading"
    : !report
      ? "never"
      : data.stale
        ? "stale"
        : "ok";

  const badge = (() => {
    if (state === "never") {
      return (
        <Badge variant="destructive" className="gap-1">
          <AlertTriangle className="h-3 w-3" />
          {t("versions_status_never")}
        </Badge>
      );
    }
    if (state === "stale") {
      return (
        <Badge variant="destructive" className="gap-1">
          <AlertTriangle className="h-3 w-3" />
          {t("versions_status_stale")}
        </Badge>
      );
    }
    if (state !== "ok") return null;
    return available.length > 0 ? (
      <Badge className="gap-1 border-transparent bg-amber-500/15 text-amber-600 dark:text-amber-400">
        <ArrowUpCircle className="h-3 w-3" />
        {t("versions_status_available", { count: available.length })}
      </Badge>
    ) : (
      <Badge className="gap-1 border-transparent bg-emerald-500/15 text-emerald-500">
        <CheckCircle2 className="h-3 w-3" />
        {t("versions_status_current")}
      </Badge>
    );
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
                : available.length > 0
                  ? "bg-amber-500/10 text-amber-500"
                  : "bg-primary/10 text-primary",
            )}
          >
            <PackageCheck className="h-5 w-5" />
          </div>
          <div className="min-w-0">
            <CardTitle className="flex flex-wrap items-center gap-2 text-base font-semibold">
              {t("versions_title")}
              {badge}
            </CardTitle>
            <p className="mt-0.5 text-xs text-muted-foreground">{t("versions_subtitle")}</p>
          </div>
        </div>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="h-8 w-8 shrink-0"
          onClick={reload}
          disabled={loading}
          aria-label={t("versions_refresh")}
          title={t("versions_refresh")}
        >
          <RefreshCw className={cn("h-4 w-4", loading && "animate-spin")} />
        </Button>
      </CardHeader>

      <CardContent className="space-y-4 text-sm">
        {state === "loading" && (
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            {t("versions_loading")}
          </div>
        )}

        {state === "error" && <MessageLine kind="error" message={humanizeError(error)} />}

        {report && (
          <>
            <p className="text-xs text-muted-foreground">
              <span title={formatAbsolute(report.checked_at, locale)} className={cn(state === "stale" && "text-destructive")}>
                {t("versions_checked", { when: formatRelative(report.checked_at, Date.now(), locale), felis: report.felis })}
              </span>
            </p>
            {report.components.length === 0 ? (
              <p className="text-xs text-muted-foreground">{t("versions_none")}</p>
            ) : (
              <div className="rounded-md border border-border">
                <div className={cn("hidden border-b px-3 py-2 text-[11px] font-semibold text-muted-foreground", GRID)}>
                  <div>{t("versions_col_component")}</div>
                  <div>{t("versions_col_installed")}</div>
                  <div>{t("versions_col_latest")}</div>
                  <div>{t("versions_col_status")}</div>
                </div>
                <ul className="divide-y divide-border">
                  {report.components.map((c) => (
                    <ComponentRow key={c.name} c={c} />
                  ))}
                </ul>
              </div>
            )}
          </>
        )}

        {state === "ok" && available.length > 0 && (
          <div className="space-y-2 rounded-lg border border-amber-500/25 bg-amber-500/5 p-4">
            <p className="text-xs leading-relaxed text-foreground">{t("versions_apply_hint")}</p>
            <CopyCommand command={`sudo felis update ${selectors.map((s) => `--${s}`).join(" ")}`.trimEnd()} />
          </div>
        )}

        {(state === "stale" || state === "never") && (
          <div className="space-y-3 rounded-lg border border-destructive/25 bg-destructive/5 p-4">
            <div className="flex items-start gap-2 text-destructive">
              <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
              <div className="space-y-1">
                <p className="font-semibold">
                  {state === "never" ? t("versions_never_title") : t("versions_stale_title", { hours: maxAgeHours })}
                </p>
                <p className="text-xs leading-relaxed text-destructive/90">{t("versions_fix_hint")}</p>
              </div>
            </div>
            <div className="space-y-2">
              {FIX_COMMANDS.map((cmd) => (
                <CopyCommand key={cmd} command={cmd} />
              ))}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function ComponentRow({ c }: { c: UpdateComponent }) {
  const { t } = useTranslation("admin");
  return (
    <li className="px-3 py-2.5 text-xs">
      <div className={cn("flex flex-wrap items-center gap-x-3 gap-y-1", GRID)}>
        <div className="w-full font-semibold text-foreground sm:w-auto">{c.name}</div>
        <div className="min-w-0 truncate font-mono text-muted-foreground" title={c.current}>
          <span className="sm:hidden">{t("versions_col_installed")}: </span>
          {c.current ?? "—"}
        </div>
        <div className="min-w-0 truncate font-mono" title={c.latest}>
          <span className="text-muted-foreground sm:hidden">{t("versions_col_latest")}: </span>
          {c.latest ? <span className="text-amber-600 dark:text-amber-400">{c.latest}</span> : <span className="text-muted-foreground">—</span>}
        </div>
        <div>
          <span className={cn("inline-block rounded border px-1.5 py-px text-[10px] font-semibold", STATE_STYLE[c.state])}>
            {t(`versions_state_${c.state}`)}
          </span>
        </div>
      </div>
      {c.error && <p className="mt-1 break-words text-[11px] leading-relaxed text-rose-500/90">{c.error}</p>}
      {c.note && <p className="mt-1 break-words text-[11px] leading-relaxed text-muted-foreground">{c.note}</p>}
    </li>
  );
}
