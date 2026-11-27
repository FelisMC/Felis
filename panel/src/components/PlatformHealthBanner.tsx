import { ArrowRight } from "lucide-react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "@/lib/api";
import { formatBytes, formatRelative } from "@/lib/format";
import { useAsync, usePolling } from "@/lib/hooks";
import type { OffsiteStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

// Hide host-health summaries when host state cannot be read.
export function PlatformHealthBanner({ inline = false }: { inline?: boolean }) {
  const { t, i18n } = useTranslation("admin");
  const overview = useAsync(api.hostOverview, []);
  usePolling(overview.reload, 60_000);
  const data = overview.data;
  if (!data) return null;
  const now = Date.now();
  const newest = data.databaseBackups[0];
  const disk = [...data.disks].sort((a, b) => a.available / a.total - b.available / b.total)[0];
  const diskFree = disk ? disk.available / disk.total : 1;
  const status = data.offsite.status as OffsiteStatus | undefined;
  const offsiteOk = data.offsite.configured && !!status?.last_success && !status.key_mismatch && !status.standby && !status.displaced;
  const open = data.alerts.filter((a) => !a.ignored).length;
  const items = [
    { key: "alerts", dot: true, bad: open > 0, text: open ? t("host_alerts_open", { count: open }) : t("host_alerts_none"), tab: "status" },
    { key: "db", dot: !newest, bad: !newest, text: newest ? t("health_db_newest", { time: formatRelative(newest.created, now, i18n.language) }) : t("db_bundles_none"), tab: "backups" },
    { key: "offsite", dot: !offsiteOk, bad: !offsiteOk, text: t("health_offsite", { state: t(!data.offsite.configured ? "offsite_state_off" : offsiteOk ? "offsite_state_ok" : "offsite_state_attention") }), tab: "offsite" },
    ...(disk ? [{ key: "disk", dot: diskFree < 0.1, bad: diskFree < 0.1, text: t("health_disk_free", { free: formatBytes(disk.available), path: disk.path }), tab: "status" }] : []),
  ];

  if (inline) {
    return (
      <section aria-label={t("health_title")} className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        {items.map(({ key, dot, bad, text, tab }) => (
          <Link
            key={key}
            to={`/admin/updates?tab=${tab}`}
            className={cn(
              "inline-flex items-center gap-1.5 transition-colors hover:underline",
              bad ? "text-destructive" : "text-muted-foreground hover:text-foreground",
              key === "disk" && "font-mono",
            )}
          >
            {dot && (
              <span
                aria-hidden="true"
                className={cn("h-[5.5px] w-[5.5px] shrink-0 rounded-full", bad ? "bg-destructive" : "bg-primary")}
              />
            )}
            <span>{text}</span>
          </Link>
        ))}
      </section>
    );
  }

  return (
    <section
      aria-label={t("health_title")}
      className="flex flex-wrap items-center gap-x-3 gap-y-1.5 rounded-lg border border-border bg-card px-3.5 py-2 text-xs"
    >
      {items.map(({ key, dot, bad, text, tab }) => (
        <Link
          key={key}
          to={`/admin/updates?tab=${tab}`}
          className={cn(
            "inline-flex items-center gap-1.5 transition-colors hover:underline",
            bad ? "font-medium text-destructive" : "text-muted-foreground hover:text-foreground",
          )}
        >
          {dot && (
            <span
              aria-hidden="true"
              className={cn("h-[5.5px] w-[5.5px] shrink-0 rounded-full", bad ? "bg-destructive" : "bg-primary")}
            />
          )}
          <span>{text}</span>
        </Link>
      ))}
      <Link
        to="/admin/updates"
        className="ml-auto inline-flex h-6 items-center gap-1 rounded-md border border-border bg-background px-2 text-[11px] font-medium text-foreground transition-colors hover:border-muted-foreground/30 hover:bg-accent/70"
      >
        {t("health_open")}
        <ArrowRight className="h-3 w-3" />
      </Link>
    </section>
  );
}
