import { AlertTriangle, ArrowRight, CheckCircle2, CloudUpload, Database, HardDrive } from "lucide-react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "@/lib/api";
import { formatBytes, formatRelative } from "@/lib/format";
import { useAsync, usePolling } from "@/lib/hooks";
import type { OffsiteStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

/** PlatformHealthBanner is the Owner's line on the dashboard about the host: open alerts,
 *  the newest database backup, the off-site copy and the fullest disk, linking to the
 *  maintenance page. It stays out of the way when the host cannot be read. */
export function PlatformHealthBanner() {
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
  const items = [
    { key: "alerts", icon: data.alerts.length ? AlertTriangle : CheckCircle2, bad: data.alerts.length > 0, text: data.alerts.length ? t("host_alerts_open", { count: data.alerts.length }) : t("host_alerts_none"), tab: "status" },
    { key: "db", icon: Database, bad: !newest, text: newest ? t("health_db_newest", { time: formatRelative(newest.created, now, i18n.language) }) : t("db_bundles_none"), tab: "backups" },
    { key: "offsite", icon: CloudUpload, bad: !offsiteOk, text: t("health_offsite", { state: t(!data.offsite.configured ? "offsite_state_off" : offsiteOk ? "offsite_state_ok" : "offsite_state_attention") }), tab: "offsite" },
    ...(disk ? [{ key: "disk", icon: HardDrive, bad: diskFree < 0.1, text: t("health_disk_free", { free: formatBytes(disk.available), path: disk.path }), tab: "status" }] : []),
  ];
  return (
    <section aria-label={t("health_title")} className="flex flex-wrap items-center gap-x-5 gap-y-2 rounded-xl border border-border bg-card px-4 py-3">
      {items.map(({ key, icon: Icon, bad, text, tab }) => (
        <Link key={key} to={`/admin/updates?tab=${tab}`} className={cn("flex items-center gap-2 text-sm hover:underline", bad ? "text-destructive" : "text-foreground")}>
          <Icon className={cn("h-4 w-4", bad ? "text-destructive" : "text-primary")} aria-hidden="true" />{text}
        </Link>
      ))}
      <Link to="/admin/updates" className="ml-auto inline-flex items-center gap-1 text-xs font-medium text-primary hover:underline">{t("health_open")}<ArrowRight className="h-3.5 w-3.5" /></Link>
    </section>
  );
}
