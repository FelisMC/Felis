import { useState, useEffect } from "react";
import { Clock, Loader2, Calendar, Globe } from "lucide-react";
import { MessageLine } from "@/components/MessageLine";
import { StatCard } from "@/components/StatCard";
import { PageHeader } from "@/components/PageHeader";
import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Loading, ErrorState } from "@/components/States";
import { api, humanizeError } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { formatAbsolute } from "@/lib/format";
import { DBBackupCard } from "./DBBackupCard";
import { UpdateReportCard } from "./UpdateReportCard";

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

export function UpdatesPage() {
  const { t, i18n } = useTranslation("admin");
  const locale = i18n.language;
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;

  const { data, error, loading, reload } = useAsync(() => api.getUpdateWindow(), []);

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
      <PageHeader icon={Clock} title={t("updates_title")} subtitle={t("updates_subtitle")} />

      {/* Control-plane database backup freshness (read-only, host timer) */}
      <DBBackupCard />

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
              <div className="space-y-1.5">
                <Label htmlFor="startTime" className="text-xs font-semibold text-muted-foreground">{t("updates_start_label")} *</Label>
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

              <div className="space-y-1.5">
                <Label htmlFor="endTime" className="text-xs font-semibold text-muted-foreground">{t("updates_end_label")} *</Label>
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
