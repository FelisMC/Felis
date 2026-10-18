import { useState } from "react";
import { AlertTriangle, CheckCircle2, CircleSlash, Download, FileArchive, Loader2, ScrollText, Stethoscope } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { HostTaskNotice, HostTaskStatus, useHostTask } from "@/components/HostTask";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { saveBlob } from "@/lib/download";
import { formatAbsolute } from "@/lib/format";
import type { DoctorView } from "@/lib/types";
import { cn } from "@/lib/utils";

/** DoctorCard runs every health check once, as `felis doctor` does, and shows what it
 *  found by area: what is wrong, how serious, and where to look next. It mails nothing. */
export function DoctorCard() {
  const { t, i18n } = useTranslation("admin");
  const host = useHostTask(["doctor"]);
  const report = host.task?.state === "succeeded" ? (host.task.result as unknown as DoctorView | undefined) : undefined;
  const zh = i18n.language.startsWith("zh");
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-center gap-2"><Stethoscope className="h-4 w-4 text-primary" aria-hidden="true" />{t("doctor_title")}</CardTitle>
        {report && <Badge variant={report.critical ? "destructive" : report.warning ? "muted" : "default"}>{report.critical + report.warning ? t("doctor_problems", { critical: report.critical, warning: report.warning }) : t("doctor_clean")}</Badge>}
      </CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("doctor_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        <Button disabled={!host.available || host.busy} onClick={() => void host.start({ action: "doctor", confirmMaintenance: false })}>
          {host.running || host.starting ? <Loader2 className="animate-spin" /> : <Stethoscope />}{t("doctor_run")}
        </Button>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {report && (
          <section aria-label={t("doctor_report")} className="space-y-3">
            <p className="text-xs text-muted-foreground">{t("doctor_at", { time: formatAbsolute(report.at, i18n.language), host: report.host })}</p>
            <ul className="divide-y divide-border rounded-lg border border-border">
              {report.areas.map((area) => (
                <li key={area.key} className="space-y-2 p-3">
                  <div className="flex items-center gap-2 text-sm">
                    {area.state === "ok" && <CheckCircle2 className="h-4 w-4 text-primary" aria-hidden="true" />}
                    {area.state === "skipped" && <CircleSlash className="h-4 w-4 text-muted-foreground" aria-hidden="true" />}
                    {area.state === "problem" && <AlertTriangle className={cn("h-4 w-4", area.findings?.some((f) => f.severity === "critical") ? "text-destructive" : "text-amber-500")} aria-hidden="true" />}
                    <span className="font-medium">{t(`doctor_area_${area.key}`, { defaultValue: area.title })}</span>
                    {area.state === "skipped" && <span className="text-xs text-muted-foreground">{t("doctor_skipped", { reason: area.skipped })}</span>}
                  </div>
                  {area.findings?.map((f) => (
                    <div key={f.key} className="ml-6 space-y-1">
                      <p className="text-sm"><Badge variant={f.severity === "critical" ? "destructive" : "muted"} className="mr-2">{t(`host_severity_${f.severity}`)}</Badge>{(zh ? f.summary : f.summary_en) || f.summary_en || f.summary}</p>
                      <p className="text-xs text-muted-foreground"><code>{f.key}</code></p>
                      {f.hint && <p className="text-xs leading-relaxed text-muted-foreground">{f.hint}</p>}
                    </div>
                  ))}
                </li>
              ))}
            </ul>
            {report.notes?.map((n) => <p key={n} className="text-xs leading-relaxed text-muted-foreground">{n}</p>)}
          </section>
        )}
        {host.task && host.task.state !== "succeeded" && <HostTaskStatus task={host.task} />}
      </CardContent>
      {host.reauth.dialog}
    </Card>
  );
}

// Game logs contain player data and require explicit inclusion.
export function SupportBundleCard() {
  const { t } = useTranslation("admin");
  const host = useHostTask(["support-bundle"]);
  const [serverLogs, setServerLogs] = useState(false);
  return (
    <Card>
      <CardHeader><CardTitle className="flex items-center gap-2"><FileArchive className="h-4 w-4 text-primary" aria-hidden="true" />{t("support_title")}</CardTitle></CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("support_description")}</p>
        <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
        <div className="flex items-start justify-between gap-4 rounded-lg border border-border p-4">
          <div className="space-y-1"><Label htmlFor="support-server-logs">{t("support_server_logs")}</Label><p className="text-xs leading-relaxed text-muted-foreground">{t("support_server_logs_hint")}</p></div>
          <Switch id="support-server-logs" checked={serverLogs} disabled={host.busy} onCheckedChange={setServerLogs} />
        </div>
        <Button disabled={!host.available || host.busy} onClick={() => void host.start({ action: "support-bundle", params: { serverLogs: String(serverLogs) }, confirmMaintenance: false })}>
          {host.running || host.starting ? <Loader2 className="animate-spin" /> : <FileArchive />}{t("support_create")}
        </Button>
        {host.error && <MessageLine kind="error" message={host.error} />}
        {host.task && <HostTaskStatus task={host.task} />}
      </CardContent>
      {host.reauth.dialog}
    </Card>
  );
}

/** AuditExportCard downloads the audit rows of a period as JSON lines, so an install can
 *  keep them past the audit retention, after which they are deleted. */
export function AuditExportCard() {
  const { t } = useTranslation("admin");
  const reauth = useReauth();
  const [since, setSince] = useState("");
  const [until, setUntil] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const ordered = !since || !until || until > since;
  async function exportRows() {
    setBusy(true);
    setError(null);
    try {
      const file = await reauth.guard(() => api.exportAudit(since, until));
      saveBlob(file.blob, file.filename);
    } catch (e) {
      if (!isReauthCancelled(e)) setError(humanizeError(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Card>
      <CardHeader><CardTitle className="flex items-center gap-2"><ScrollText className="h-4 w-4 text-primary" aria-hidden="true" />{t("audit_export_title")}</CardTitle></CardHeader>
      <CardContent className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{t("audit_export_description")}</p>
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-2"><Label htmlFor="audit-since">{t("audit_export_since")}</Label><Input id="audit-since" type="date" value={since} disabled={busy} onChange={(e) => setSince(e.target.value)} /></div>
          <div className="space-y-2"><Label htmlFor="audit-until">{t("audit_export_until")}</Label><Input id="audit-until" type="date" value={until} disabled={busy} onChange={(e) => setUntil(e.target.value)} /></div>
        </div>
        <p className="text-xs text-muted-foreground">{t("audit_export_bounds")}</p>
        {!ordered && <MessageLine kind="error" message={t("audit_export_order")} />}
        <Button disabled={busy || !ordered} onClick={() => void exportRows()}>{busy ? <Loader2 className="animate-spin" /> : <Download />}{t("audit_export_download")}</Button>
        {error && <MessageLine kind="error" message={error} />}
      </CardContent>
      {reauth.dialog}
    </Card>
  );
}
