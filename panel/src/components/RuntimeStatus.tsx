import { AlertTriangle } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { versionLabel } from "@/lib/config";
import { useConfig } from "@/lib/hooks";
import { cn } from "@/lib/utils";

export function VersionBadge({ className }: { className?: string }) {
  const { t } = useTranslation("common");
  const build = useConfig()?.build;
  return (
    <span
      className={cn(
        "inline-flex min-w-0 items-center gap-1 whitespace-nowrap font-mono text-[10px] text-muted-foreground/60",
        className,
      )}
      title={build?.version}
    >
      <span className="min-w-0 select-all truncate">{build ? versionLabel(build) : t("brand_name")}</span>
      {build?.dev && (
        <span className="shrink-0 rounded bg-amber-500/15 px-1 text-[9px] font-semibold uppercase text-amber-700 dark:text-amber-300">
          {t("build_dev")}
        </span>
      )}
    </span>
  );
}

// Missing runtime configuration leaves build defaults active; reload retries without blocking the panel.
export function ConfigBanner({ className }: { className?: string }) {
  const { t } = useTranslation("common");
  const cfg = useConfig();
  if (!cfg?.fallback) return null;
  return (
    <div
      role="alert"
      className={cn(
        "flex flex-wrap items-center justify-between gap-2 border-amber-500/30 bg-amber-500/10 px-4 py-2 text-sm text-amber-800 dark:text-amber-200",
        className ?? "border-b",
      )}
    >
      <span className="flex min-w-0 items-center gap-2">
        <AlertTriangle className="h-4 w-4 shrink-0" />
        {t("config_unavailable")}
      </span>
      <Button size="sm" variant="outline" onClick={() => window.location.reload()}>
        {t("reload_page")}
      </Button>
    </div>
  );
}
