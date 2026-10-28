import { Activity, HardDrive } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { api } from "@/lib/api";
import { useAsync, usePolling } from "@/lib/hooks";
import { formatAbsolute, formatBytes } from "@/lib/format";
import { cpuUsage, memoryLimit, memoryUsage } from "@/lib/resourceUsage";
import type { ServerDiskUsage } from "@/lib/types";

export function DiskUsage({ disk, diskFull = false }: { disk?: ServerDiskUsage; diskFull?: boolean }) {
  const { t, i18n } = useTranslation("servers");
  const fraction = disk ? disk.usedBytes / disk.limitBytes : 0;
  const value = disk ? t("disk_value", { used: formatBytes(disk.usedBytes), limit: formatBytes(disk.limitBytes) }) : "—";
  return <section aria-label={t("disk_title")} className="min-w-0 space-y-2">
    <div className="flex flex-wrap items-baseline justify-between gap-2 text-sm">
      <h3 className="flex items-center gap-2 font-medium"><HardDrive className="h-4 w-4 text-muted-foreground" />{t("disk_title")}</h3>
      <p className="tabular-nums">{value}</p>
    </div>
    {disk ? <>
      <div role="meter" aria-label={t("disk_title")} aria-valuemin={0} aria-valuemax={disk.limitBytes} aria-valuenow={Math.min(disk.usedBytes, disk.limitBytes)} aria-valuetext={value} className="h-2 overflow-hidden rounded-full bg-muted">
        <div className={`h-full rounded-full ${diskFull ? "bg-destructive" : fraction >= 0.9 ? "bg-amber-500" : "bg-primary"}`} style={{ width: `${Math.min(100, fraction * 100)}%` }} />
      </div>
      <p className="text-xs text-muted-foreground">{t(disk.stale ? "disk_stale" : "disk_sampled")} <time dateTime={disk.sampledAt}>{formatAbsolute(disk.sampledAt, i18n.language)}</time></p>
    </> : <p className="text-xs text-muted-foreground">{t("disk_unknown")}</p>}
    {diskFull ? <p role="status" className="text-xs text-destructive">{t("disk_full_blocked")}</p>
      : fraction >= 0.9 && <p role="status" className="text-xs text-amber-700 dark:text-amber-400">{t("disk_warning")}</p>}
  </section>;
}

type ChartMetric = "cpuMilli" | "memoryBytes" | "running";
type ChartPoint = { at: string } & Partial<Record<ChartMetric, number>>;

/** UsageChart draws the last hour of one metric; the vertical scale reaches the larger of limit and the highest point. */
export function UsageChart({ history, metric, label, limit }: { history: ChartPoint[]; metric: ChartMetric; label: string; limit: number }) {
  const now = Date.now();
  const points = history.filter((point) => Date.parse(point.at) >= now - 3600_000);
  const max = Math.max(limit, ...points.map((point) => point[metric] ?? 0), 1);
  const segments: string[][] = [];
  let previous = 0;
  for (const point of points) {
    const at = Date.parse(point.at);
    if (segments.length === 0 || at - previous > 45_000) segments.push([]);
    const x = Math.max(0, Math.min(300, (at - now + 3600_000) / 12_000));
    segments.at(-1)!.push(`${x.toFixed(1)},${(56 - (point[metric] ?? 0) / max * 52).toFixed(1)}`);
    previous = at;
  }
  const last = segments.at(-1)?.at(-1)?.split(",");
  return <svg role="img" aria-label={label} viewBox="0 0 300 60" className="h-16 w-full text-primary" preserveAspectRatio="none">
    <path d="M0 4H300 M0 30H300 M0 56H300" className="stroke-border" fill="none" strokeWidth="0.5" />
    {segments.map((segment, i) => <polyline key={i} points={segment.join(" ")} fill="none" stroke="currentColor" strokeWidth="1.5" vectorEffect="non-scaling-stroke" />)}
    {last && <circle cx={last[0]} cy={last[1]} r="2" fill="currentColor" />}
  </svg>;
}

export function ResourceUsage({ name, running, cpu, memory, disk, diskFull }: { name: string; running: boolean; cpu?: string; memory?: string; disk?: ServerDiskUsage; diskFull?: boolean }) {
  const { t } = useTranslation("servers");
  const { data, error, reload } = useAsync(() => api.serverUsage(name), [name], { coalesce: true });
  usePolling(reload, 15_000);
  const current = running && !error && data?.current && Date.parse(data.current.at) >= Date.now() - 45_000 ? data.current : null;
  const history = data?.history ?? [];
  const cpuLimit = cpu?.endsWith("m") ? Number(cpu.slice(0, -1)) : Number(cpu) * 1000;
  return <Card role="region" aria-label={t("usage_title")}>
    <CardContent className="space-y-3 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="flex items-center gap-2 text-sm font-medium"><Activity className="h-4 w-4 text-muted-foreground" />{t("usage_title")}</h2>
        <p className="text-xs text-muted-foreground">{t("usage_window")}</p>
      </div>
      <dl className="grid gap-4 sm:grid-cols-2">
        <div className="min-w-0 space-y-2">
          <div className="flex items-baseline justify-between gap-2 text-sm"><dt>{t("usage_cpu")}</dt><dd className="tabular-nums">{cpuUsage(current?.cpuMilli, cpu)}</dd></div>
          <UsageChart history={history} metric="cpuMilli" label={t("usage_cpu_curve")} limit={cpuLimit || 1000} />
        </div>
        <div className="min-w-0 space-y-2">
          <div className="flex flex-wrap items-baseline justify-between gap-2 text-sm"><dt>{t("usage_memory")}</dt><dd className="tabular-nums">{memoryUsage(current?.memoryBytes, memory)}</dd></div>
          <UsageChart history={history} metric="memoryBytes" label={t("usage_memory_curve")} limit={memoryLimit(memory) ?? 1} />
        </div>
      </dl>
      <p aria-live="polite" className="text-xs text-muted-foreground">{t(!running ? "usage_stopped" : !current ? "usage_unavailable" : "usage_cpu_basis")}</p>
      <div className="border-t border-border pt-3"><DiskUsage disk={disk} diskFull={diskFull} /></div>
    </CardContent>
  </Card>;
}
