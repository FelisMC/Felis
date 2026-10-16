import { useState, useEffect } from "react";
import { Activity, Archive, Calendar, Clock, CloudUpload, Globe, Loader2, PackageCheck, RefreshCw, Stethoscope } from "lucide-react";
import { useSearchParams } from "react-router-dom";
import { useHostTask } from "@/components/HostTask";
import { MessageLine } from "@/components/MessageLine";
import { StatCard } from "@/components/StatCard";
import { PageHeader } from "@/components/PageHeader";
import { PageTabs } from "@/components/PageTabs";
import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Loading, ErrorState } from "@/components/States";
import { api, humanizeError } from "@/lib/api";
import { RefreshContext, useAsync, usePageRefresh, useRefreshSignal } from "@/lib/hooks";
import { formatAbsolute } from "@/lib/format";
import { DBBackupCard } from "./DBBackupCard";
import { UpdateReportCard } from "./UpdateReportCard";
import { HostStatusCard } from "./maintenance/HostStatusCard";
import { DatabaseBundlesCard } from "./maintenance/DatabaseBundlesCard";
import { WorldBackupCard } from "./maintenance/WorldBackupCard";
import { OffsiteCard, RestoreHostCard } from "./maintenance/OffsiteCard";
import { AuditExportCard, DoctorCard, SupportBundleCard } from "./maintenance/DiagnosticsCards";

function toLocalDatetimeString(dateOrStr: Date | string | null | undefined): string {
  if (!dateOrStr) return "";
  const d = new Date(dateOrStr);
  if (isNaN(d.getTime())) return "";
  
  const YYYY = d.getFullYear();
  const MM = String(d.getMonth() + 1).padStart(2, "0");
  const DD = String(d.getDate()).padStart(2, "0");
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  return `${YYYY}-${MM}-${DD}T${hh}:${mm}`;
}

// UpdatesSection is the update tab: the versions the host found and the maintenance
// window `felis update --apply` waits for. Applying stays on the host.
function UpdatesSection() {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;

  const { data, error, loading, reload } = useAsync(() => api.getUpdateWindow(), []);
  useRefreshSignal(reload);

  const [startVal, setStartVal] = useState("");
  const [endVal, setEndVal] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [successMsg, setSuccessMsg] = useState<string | null>(null);

  useEffect(() => {
    if (data) {
      setStartVal(toLocalDatetimeString(data.start));
      setEndVal(toLocalDatetimeString(data.end));
    }
  }, [data]);

  const windowStatus = (() => {
    if (!data || !data.start || !data.end) return "unset";
    const now = new Date();
    const start = new Date(data.start);
    const end = new Date(data.end);
    if (now < start) return "pending";
    if (now >= start && now < end) return "active";
    return "expired";
  })();

  async function handleClear() {
    if (clearing) return;
    setClearing(true);
    setActionError(null);
    setSuccessMsg(null);
    try {
      await api.setUpdateWindow({ start: null, end: null });
      setStartVal("");
      setEndVal("");
      setSuccessMsg(t("updates_clear_success"));
      reload();
    } catch (err) {
      setActionError(humanizeError(err));
    } finally {
      setClearing(false);
    }
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    if (submitting) return;
    setActionError(null);
    setSuccessMsg(null);

    if (!startVal && !endVal) {
      handleClear();
      return;
    }

    if (!startVal || !endVal) {
      setActionError(t("updates_validation_both"));
      return;
    }

    const startD = new Date(startVal);
    const endD = new Date(endVal);

    if (endD <= startD) {
      setActionError(t("updates_validation_order"));
      return;
    }

    setSubmitting(true);
    try {
      await api.setUpdateWindow({
        start: startD.toISOString(),
        end: endD.toISOString(),
      });
      setSuccessMsg(t("updates_save_success"));
      reload();
    } catch (err) {
      setActionError(humanizeError(err));
    } finally {
      setSubmitting(false);
    }
  }

  if (loading && !data) {
    return <Loading />;
  }

  if (error) {
    return <ErrorState error={error} onRetry={reload} />;
  }

  const statusText = (() => {
    switch (windowStatus) {
      case "active":
        return t("updates_status_active");
      case "pending":
        return t("updates_status_pending");
      case "expired":
        return t("updates_status_expired");
      default:
        return t("updates_status_unset");
    }
  })();

  const statusColorClass = (() => {
    switch (windowStatus) {
      case "active":
        return "text-emerald-500";
      case "pending":
        return "text-blue-500";
      case "expired":
        return "text-zinc-500";
      default:
        return "text-amber-500";
    }
  })();

  const statusAccentClass = (() => {
    switch (windowStatus) {
      case "active":
        return "text-emerald-500 bg-emerald-500/10";
      case "pending":
        return "text-blue-500 bg-blue-500/10";
      case "expired":
        return "text-zinc-500 bg-zinc-500/10";
      default:
        return "text-amber-500 bg-amber-500/10";
    }
  })();

  return (
    <div className="space-y-6">
      {/* Installed vs newest upstream versions (read-only, host timer) */}
      <UpdateReportCard />

      {/* Stats Cards Row */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-4">
        <StatCard
          icon={Clock}
          label={t("updates_stat_status")}
          value={statusText}
          accentClass={statusAccentClass}
          valueClass={cn("text-sm sm:text-base font-bold font-mono leading-tight truncate", statusColorClass)}
        />
        <StatCard
          icon={Calendar}
          label={t("updates_stat_start")}
          value={data?.start ? formatAbsolute(data.start, locale) : "—"}
          accentClass="text-primary bg-primary/10"
          valueClass="text-sm sm:text-base font-bold font-mono leading-tight truncate"
        />
        <StatCard
          icon={Calendar}
          label={t("updates_stat_end")}
          value={data?.end ? formatAbsolute(data.end, locale) : "—"}
          accentClass="text-primary bg-primary/10"
          valueClass="text-sm sm:text-base font-bold font-mono leading-tight truncate"
        />
        <StatCard
          icon={Globe}
          label={t("updates_stat_timezone")}
          value={tz}
          accentClass="text-primary bg-primary/10"
          valueClass="text-sm sm:text-base font-bold font-mono leading-tight truncate"
        />
      </div>

      {/* Configuration Card */}
      <Card className="w-full">
        <CardHeader>
          <CardTitle>{t("updates_set_title")}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-6 text-sm">
          <p className="text-muted-foreground leading-relaxed">{t("updates_window_advisory")}</p>
          {/* Unset hint warning */}
          {windowStatus === "unset" && (
            <p className="text-muted-foreground bg-muted/15 border border-dashed border-border rounded-lg p-4 leading-relaxed">
              {t("updates_current_unset")}
            </p>
          )}

          <form onSubmit={handleSubmit} className="space-y-5">
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div className="space-y-2">
                <Label htmlFor="startTime">{t("updates_start_label")} *</Label>
                <Input
                  id="startTime"
                  type="datetime-local"
                  value={startVal}
                  onChange={(e) => setStartVal(e.target.value)}
                  disabled={submitting || clearing}
                  className="w-full"
                  required
                />
              </div>

              <div className="space-y-2">
                <Label htmlFor="endTime">{t("updates_end_label")} *</Label>
                <Input
                  id="endTime"
                  type="datetime-local"
                  value={endVal}
                  onChange={(e) => setEndVal(e.target.value)}
                  disabled={submitting || clearing}
                  className="w-full"
                  required
                />
              </div>
            </div>

            <div className="text-[11px] text-muted-foreground leading-normal bg-muted/15 border border-border/40 rounded p-3">
              {t("updates_timezone_hint", { tz })}
            </div>

            {actionError && (
              <MessageLine kind="error" message={actionError} />
            )}

            {successMsg && (
              <MessageLine kind="success" message={successMsg} />
            )}

            <div className="flex flex-wrap gap-3 pt-2">
              <Button
                type="submit"
                disabled={submitting || clearing}
                className="text-xs px-4"
              >
                {submitting ? (
                  <>
                    <Loader2 className="h-4 w-4 animate-spin mr-1.5" />
                    {t("common:creating", "Saving...")}
                  </>
                ) : (
                  t("updates_submit_btn")
                )}
              </Button>

              {windowStatus !== "unset" && (
                <Button
                  type="button"
                  variant="outline"
                  className="text-xs px-4 text-destructive hover:text-destructive hover:bg-destructive/10 border-input hover:border-destructive/20"
                  disabled={submitting || clearing}
                  onClick={handleClear}
                >
                  {clearing ? (
                    <>
                      <Loader2 className="h-4 w-4 animate-spin mr-1.5" />
                      {t("common:creating", "Processing...")}
                    </>
                  ) : (
                    t("updates_clear_btn")
                  )}
                </Button>
              )}
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}

const tabs = [
  { key: "status", icon: Activity },
  { key: "backups", icon: Archive },
  { key: "offsite", icon: CloudUpload },
  { key: "diagnostics", icon: Stethoscope },
  { key: "updates", icon: PackageCheck },
] as const;
type Tab = (typeof tabs)[number]["key"];

// UpdatesPage is the Owner's maintenance page: the host's state, backups, the off-site
// copy, diagnostics and updates, each a tab. Every tab stays mounted, so a draft on one
// survives a look at another; the header's reload rereads all of them.
export function UpdatesPage() {
  const { t } = useTranslation("admin");
  const [params, setParams] = useSearchParams();
  const requested = params.get("tab");
  const tab: Tab = tabs.some((x) => x.key === requested) ? (requested as Tab) : "status";
  const cards = usePageRefresh();
  const overview = useAsync(api.hostOverview, []);
  const db = useHostTask(["db-backup", "db-verify", "db-delete", "db-restore"]);
  return (
    <RefreshContext.Provider value={cards.scope}>
      <div className="space-y-6">
        <PageHeader icon={Clock} title={t("updates_title")} subtitle={t("updates_subtitle")}
          actions={<Button variant="outline" size="sm" disabled={cards.dirty || overview.loading} onClick={() => { overview.reload(); cards.refresh(); }}><RefreshCw />{t("platform_reload")}</Button>}
          tabs={<PageTabs label={t("maintenance_tabs")} idPrefix="maintenance" value={tab} tabs={tabs.map(({ key, icon }) => ({ key, icon, label: t(`maintenance_tab_${key}`) }))}
            onChange={(key) => setParams((p) => { p.set("tab", key); return p; }, { replace: true })} />} />
        <div role="tabpanel" id="maintenance-panel-status" aria-labelledby="maintenance-tab-status" hidden={tab !== "status"} className="space-y-6">
          <HostStatusCard overview={overview} />
        </div>
        <div role="tabpanel" id="maintenance-panel-backups" aria-labelledby="maintenance-tab-backups" hidden={tab !== "backups"} className="space-y-6">
          <DBBackupCard offsite={overview.data?.offsite.configured} backupBusy={db.busy}
            onBackup={db.available ? () => void db.start({ action: "db-backup", confirmMaintenance: false }).then((task) => task && overview.reload()) : undefined} />
          <DatabaseBundlesCard overview={overview} db={db} />
          <WorldBackupCard />
          <AuditExportCard />
        </div>
        <div role="tabpanel" id="maintenance-panel-offsite" aria-labelledby="maintenance-tab-offsite" hidden={tab !== "offsite"} className="space-y-6">
          <OffsiteCard overview={overview} />
          <RestoreHostCard overview={overview} />
        </div>
        <div role="tabpanel" id="maintenance-panel-diagnostics" aria-labelledby="maintenance-tab-diagnostics" hidden={tab !== "diagnostics"} className="space-y-6">
          <DoctorCard />
          <SupportBundleCard />
        </div>
        <div role="tabpanel" id="maintenance-panel-updates" aria-labelledby="maintenance-tab-updates" hidden={tab !== "updates"} className="space-y-6">
          <UpdatesSection />
        </div>
      </div>
    </RefreshContext.Provider>
  );
}
