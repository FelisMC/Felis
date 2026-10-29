import { Cpu, HardDrive, MemoryStick } from "lucide-react";
import { useTranslation } from "react-i18next";
import { AreaChart } from "@/components/AreaChart";
import { MetricTile } from "@/components/MetricTile";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import { useAsync, usePolling } from "@/lib/hooks";
import { formatAbsolute, formatBytes } from "@/lib/format";
import { cpuUsage, memoryLimit, memoryUsage } from "@/lib/resourceUsage";
import type { ServerDiskUsage } from "@/lib/types";
import { cn } from "@/lib/utils";

export function DiskUsage({ disk, diskFull = false }: { disk?: ServerDiskUsage; diskFull?: boolean }) {
  const { t, i18n } = useTranslation("servers");
  const fraction = disk ? disk.usedBytes / disk.limitBytes : 0;
  const value = disk ? t("disk_value", { used: formatBytes(disk.usedBytes), limit: formatBytes(disk.limitBytes) }) : "—";
  const tone = diskFull ? "danger" : fraction >= 0.9 ? "warn" : undefined;
  return (
    <section aria-label={t("disk_title")} className="grid min-w-0">
      <MetricTile icon={HardDrive} label={t("disk_title")} value={value} muted={!disk} tone={tone}>
        <div className="space-y-2">
          {disk ? (
            <>
              <div role="meter" aria-label={t("disk_title")} aria-valuemin={0} aria-valuemax={disk.limitBytes} aria-valuenow={Math.min(disk.usedBytes, disk.limitBytes)} aria-valuetext={value} className="h-2 overflow-hidden rounded-full bg-muted">
                <div className={cn("h-full rounded-full", diskFull ? "bg-destructive" : fraction >= 0.9 ? "bg-amber-500" : "bg-primary")} style={{ width: `${Math.min(100, fraction * 100)}%` }} />
              </div>
              <p className="text-[11px] text-muted-foreground">{t(disk.stale ? "disk_stale" : "disk_sampled")} <time dateTime={disk.sampledAt}>{formatAbsolute(disk.sampledAt, i18n.language)}</time></p>
            </>
          ) : (
            <p className="text-[11px] text-muted-foreground">{t("disk_unknown")}</p>
          )}
          {diskFull ? <p role="status" className="text-xs text-destructive">{t("disk_full_blocked")}</p>
            : fraction >= 0.9 && <p role="status" className="text-xs text-amber-700 dark:text-amber-400">{t("disk_warning")}</p>}
        </div>
      </MetricTile>
    </section>
  );
}

export function ResourceUsage({ name, running, cpu, memory, disk, diskFull }: { name: string; running: boolean; cpu?: string; memory?: string; disk?: ServerDiskUsage; diskFull?: boolean }) {
  const { t } = useTranslation("servers");
  const { data, error, reload } = useAsync(() => api.serverUsage(name), [name], { coalesce: true });
  usePolling(reload, 15_000);
  const current = running && !error && data?.current && Date.parse(data.current.at) >= Date.now() - 45_000 ? data.current : null;
  const history = data?.history ?? [];
  const cpuLimit = cpu?.endsWith("m") ? Number(cpu.slice(0, -1)) : Number(cpu) * 1000;
  const cpuValue = cpuUsage(current?.cpuMilli, cpu);
  const memoryValue = memoryUsage(current?.memoryBytes, memory);
  return (
    <Card role="region" aria-label={t("usage_title")} className="shrink-0">
      <CardHeader className="flex-row flex-wrap items-center justify-between gap-2">
        <CardTitle>{t("usage_title")}</CardTitle>
        <p className="text-xs text-muted-foreground">{t("usage_window")}</p>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
          <MetricTile icon={Cpu} label={t("usage_cpu")} value={cpuValue} muted={!current}>
            <AreaChart points={history.map((p) => ({ at: p.at, value: p.cpuMilli }))} label={t("usage_cpu_curve")} max={cpuLimit || 1000} idle={!running} />
          </MetricTile>
          <MetricTile icon={MemoryStick} label={t("usage_memory")} value={memoryValue} muted={!current}>
            <AreaChart points={history.map((p) => ({ at: p.at, value: p.memoryBytes }))} label={t("usage_memory_curve")} max={memoryLimit(memory) ?? undefined} idle={!running} />
          </MetricTile>
          <DiskUsage disk={disk} diskFull={diskFull} />
        </div>
        <p aria-live="polite" className="text-xs text-muted-foreground">{t(!running ? "usage_stopped" : !current ? "usage_unavailable" : "usage_cpu_basis")}</p>
      </CardContent>
    </Card>
  );
}
