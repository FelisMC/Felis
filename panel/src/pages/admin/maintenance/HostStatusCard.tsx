import { useState } from "react";
import { AlertTriangle, Bell, BellOff, CheckCircle2, EyeOff, HardDrive, Loader2, MemoryStick, Server } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { MessageLine } from "@/components/MessageLine";
import { isReauthCancelled, useReauth } from "@/components/ReauthDialog";
import { api, humanizeError } from "@/lib/api";
import { formatAbsolute, formatBytes, formatRelative } from "@/lib/format";
import type { AsyncState } from "@/lib/hooks";
import type { HostAlert, HostOverview } from "@/lib/types";
import { cn } from "@/lib/utils";

/** Usage bars turn amber at 80 % and red at 90 %, the watchdog's own disk thresholds. */
function UsageBar({ used, total, label, icon: Icon, mono = false }: { used: number; total: number; label: string; icon: typeof HardDrive; mono?: boolean }) {
  const { t } = useTranslation("admin");
  const share = total > 0 ? Math.min(1, used / total) : 0;
  const tone = share >= 0.9 ? "bg-destructive" : share >= 0.8 ? "bg-amber-500" : "bg-primary";
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between gap-3 text-sm">
        <span className="flex min-w-0 items-center gap-2"><Icon className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden="true" /><span className={cn("truncate text-xs", mono && "font-mono")}>{label}</span></span>
        <span className="shrink-0 text-xs text-muted-foreground">{t("host_usage", { free: formatBytes(total - used), total: formatBytes(total) })}</span>
      </div>
      <div className="h-2 overflow-hidden rounded-full bg-muted" role="meter" aria-label={label} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(share * 100)}>
        <div className={cn("h-full rounded-full", tone)} style={{ width: `${Math.round(share * 100)}%` }} />
      </div>
    </div>
  );
}

const holds = [1, 4, 24] as const;

export function HostStatusCard({ overview }: { overview: AsyncState<HostOverview> }) {
  const { t, i18n } = useTranslation("admin");
  const reauth = useReauth();
  const [hold, setHold] = useState<string>("4");
  const [holding, setHolding] = useState(false);
  const [ignoring, setIgnoring] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const data = overview.data;
  const now = Date.now();
  const locale = i18n.language;
  async function setQuiet(hours: number | null) {
    setHolding(true);
    setError(null);
    try {
      await reauth.guard(() => api.holdAlerts(hours === null ? null : new Date(Date.now() + hours * 3600_000).toISOString()));
      overview.reload();
    } catch (e) {
      if (!isReauthCancelled(e)) setError(humanizeError(e));
    } finally {
      setHolding(false);
    }
  }
  async function setIgnored(key: string, ignored: boolean) {
    setIgnoring(key);
    setError(null);
    try {
      await reauth.guard(() => api.ignoreAlert(key, ignored));
      overview.reload();
    } catch (e) {
      if (!isReauthCancelled(e)) setError(humanizeError(e));
    } finally {
      setIgnoring(null);
    }
  }
  const summary = (a: HostAlert) => (locale.startsWith("zh") ? a.summary : a.summary_en) || a.summary_en || a.summary;
  const open = data?.alerts.filter((a) => !a.ignored) ?? [];
  const ignored = data?.ignored ?? [];
  const failedUnits = data?.units.filter((u) => u.active !== "active") ?? [];
  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center gap-3">
        <CardTitle className="flex items-center gap-2"><Server className="h-4 w-4 text-primary" aria-hidden="true" />{t("host_status_title")}</CardTitle>
        {data && <span className="text-xs text-muted-foreground">{data.host} · {data.version}</span>}
      </CardHeader>
      <CardContent className="space-y-6">
        {overview.loading && !data && <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="h-4 w-4 animate-spin" />{t("host_status_loading")}</p>}
        {overview.error != null && <MessageLine kind="error" message={humanizeError(overview.error)} />}
        {data && <>
          <div className="grid gap-4 md:grid-cols-2">
            {data.disks.map((d) => <UsageBar key={d.path} icon={HardDrive} mono label={d.path} used={d.total - d.available} total={d.total} />)}
            {data.memory && <UsageBar icon={MemoryStick} label={t("host_memory")} used={data.memory.total - data.memory.available} total={data.memory.total} />}
          </div>

          <section aria-label={t("host_alerts_title")} className="space-y-3">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <h3 className="flex items-center gap-2 text-sm font-semibold">
                {open.length ? <AlertTriangle className="h-4 w-4 text-destructive" aria-hidden="true" /> : <CheckCircle2 className="h-4 w-4 text-primary" aria-hidden="true" />}
                {open.length ? t("host_alerts_open", { count: open.length }) : t("host_alerts_none")}
              </h3>
              <div className="flex flex-wrap items-center gap-2">
                {data.quietUntil ? <>
                  <Badge variant="muted" className="gap-1"><BellOff className="h-3 w-3" />{t("host_quiet_until", { time: formatAbsolute(data.quietUntil, locale) })}</Badge>
                  <Button variant="outline" size="sm" disabled={holding} onClick={() => void setQuiet(null)}>{t("host_quiet_lift")}</Button>
                </> : <>
                  <Select value={hold} onValueChange={setHold} disabled={holding}>
                    <SelectTrigger className="h-8 w-32" aria-label={t("host_quiet_duration")}><SelectValue /></SelectTrigger>
                    <SelectContent>{holds.map((h) => <SelectItem key={h} value={String(h)}>{t("host_quiet_hours", { count: h })}</SelectItem>)}</SelectContent>
                  </Select>
                  <Button variant="outline" size="sm" disabled={holding} onClick={() => void setQuiet(Number(hold))}>{holding ? <Loader2 className="animate-spin" /> : <BellOff />}{t("host_quiet_hold")}</Button>
                </>}
              </div>
            </div>
            <p className="text-xs leading-relaxed text-muted-foreground">{t("host_quiet_hint")}</p>
            {data.alertsError && <MessageLine kind="error" message={t("host_alerts_unreadable", { error: data.alertsError })} />}
            {open.length > 0 && <ul className="divide-y divide-border rounded-lg border border-border">
              {open.map((a) => (
                <li key={a.key} className="flex items-start gap-3 p-3">
                  <div className="min-w-0 flex-1 space-y-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <Badge variant={a.severity === "critical" ? "destructive" : "muted"}>{t(`host_severity_${a.severity}`)}</Badge>
                      <span className="text-sm font-medium">{summary(a)}</span>
                    </div>
                    <p className="text-xs text-muted-foreground">
                      <code>{a.key}</code> · {t("host_alert_since", { time: formatRelative(a.firstSeen, now, locale) })}
                      {a.notified ? ` · ${t("host_alert_mailed", { time: formatRelative(a.notified, now, locale) })}` : ` · ${t("host_alert_pending")}`}
                    </p>
                    {a.hint && <p className="text-xs leading-relaxed text-muted-foreground">{a.hint}</p>}
                  </div>
                  {data.ignored && (
                    <Button variant="ghost" size="sm" className="shrink-0" disabled={ignoring !== null} aria-label={t("host_alert_ignore_label", { key: a.key })} onClick={() => void setIgnored(a.key, true)}>
                      {ignoring === a.key ? <Loader2 className="animate-spin" /> : <EyeOff />}{t("host_alert_ignore")}
                    </Button>
                  )}
                </li>
              ))}
            </ul>}
            {ignored.length > 0 && (
              <details className="group rounded-lg border border-border">
                <summary className="flex cursor-pointer items-center gap-2 px-3 py-2 text-sm text-muted-foreground">
                  <EyeOff className="h-4 w-4" aria-hidden="true" />{t("host_ignored_title", { count: ignored.length })}
                </summary>
                <div className="space-y-2 border-t border-border p-3">
                  <p className="text-xs leading-relaxed text-muted-foreground">{t("host_ignored_hint")}</p>
                  <ul className="divide-y divide-border">
                    {ignored.map((key) => {
                      const firing = data.alerts.find((a) => a.key === key);
                      return (
                        <li key={key} className="flex items-center gap-3 py-2">
                          <div className="min-w-0 flex-1">
                            <p className="truncate text-sm">{firing ? summary(firing) : <code>{key}</code>}</p>
                            <p className="text-xs text-muted-foreground">{firing ? <code>{key}</code> : t("host_ignored_idle")}</p>
                          </div>
                          <Button variant="outline" size="sm" className="shrink-0" disabled={ignoring !== null} aria-label={t("host_alert_restore_label", { key })} onClick={() => void setIgnored(key, false)}>
                            {ignoring === key ? <Loader2 className="animate-spin" /> : <Bell />}{t("host_alert_restore")}
                          </Button>
                        </li>
                      );
                    })}
                  </ul>
                </div>
              </details>
            )}
          </section>

          <section aria-label={t("host_units_title")} className="space-y-3">
            <h3 className="text-sm font-semibold">{failedUnits.length ? t("host_units_problem", { count: failedUnits.length }) : t("host_units_title")}</h3>
            <ul className="grid gap-2 sm:grid-cols-2">
              {data.units.map((u) => (
                <li key={u.name} className="flex items-center justify-between gap-3 rounded-md border border-border px-3 py-2">
                  <span className="min-w-0 truncate font-mono text-xs" title={u.name}>{u.name}</span>
                  <span className="flex shrink-0 items-center gap-2">
                    {u.next && <span className="hidden text-[11px] text-muted-foreground lg:inline">{t("host_unit_next", { time: formatRelative(u.next, now, locale) })}</span>}
                    <Badge variant={u.active === "active" ? "default" : "destructive"}>{t(`host_unit_${u.active}`, { defaultValue: u.active })}</Badge>
                  </span>
                </li>
              ))}
            </ul>
            {data.proxy.address && <p className="text-xs text-muted-foreground">{t(data.proxy.reachable ? "host_proxy_ok" : "host_proxy_down", { address: data.proxy.address })}</p>}
          </section>
        </>}
        {error && <MessageLine kind="error" message={error} />}
      </CardContent>
      {reauth.dialog}
    </Card>
  );
}
