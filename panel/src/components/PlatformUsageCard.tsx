import { useId, type ReactNode } from "react";
import { ArrowRight, Cpu, MemoryStick, Play, ShieldAlert, type LucideIcon } from "lucide-react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { ErrorState, Loading, RefreshError } from "@/components/States";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { useAsync, usePolling } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import type { NodeUsage, PlatformUsagePoint } from "@/lib/types";
import { cn } from "@/lib/utils";

const POLL_MS = 15_000;
const HOUR_MS = 3600_000;

type Metric = "cpuMilli" | "memoryBytes" | "running";

const cores = (milli: number) => (milli / 1000).toFixed(2);

/** distributed shows the per-node table; on a single node it would repeat the totals. */
export function PlatformUsageCard({ distributed }: { distributed: boolean }) {
  const { t } = useTranslation("dashboard");
  const { isOwner } = useTier();
  const usage = useAsync(() => api.fleetUsage(), [], { coalesce: true });
  usePolling(usage.reload, POLL_MS);
  const data = usage.data;
  const current = data?.current ?? null;
  const history = (data?.history ?? []).filter((p) => Date.parse(p.at) >= Date.now() - HOUR_MS);

  const metrics: { metric: Metric; icon: LucideIcon; label: string; curve: string; value: string | null }[] = [
    { metric: "cpuMilli", icon: Cpu, label: t("platform_cpu"), curve: t("platform_cpu_curve"), value: current && t("platform_cpu_value", { cores: cores(current.cpuMilli) }) },
    { metric: "memoryBytes", icon: MemoryStick, label: t("platform_memory"), curve: t("platform_memory_curve"), value: current && formatBytes(current.memoryBytes) },
    { metric: "running", icon: Play, label: t("platform_running"), curve: t("platform_running_curve"), value: current && String(current.running) },
  ];

  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center justify-between gap-2">
        <CardTitle>{t("platform_title")}</CardTitle>
        <p className="text-xs text-muted-foreground">{t("platform_window")}</p>
      </CardHeader>
      <CardContent className="space-y-6">
        {!data ? (
          usage.error ? <ErrorState error={usage.error} onRetry={usage.reload} /> : <Loading />
        ) : (
          <>
            {usage.error != null && <RefreshError error={usage.error} />}
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
              {metrics.map(({ metric, icon, label, curve, value }) => (
                <Tile key={metric} icon={icon} label={label} value={value ?? t("platform_no_data")} muted={value === null}>
                  <AreaChart points={history} metric={metric} label={curve} />
                </Tile>
              ))}
              <Tile
                icon={ShieldAlert}
                label={t("platform_login_failures")}
                value={String(data.loginFailures24h)}
                warn={data.loginFailures24h > 0}
              >
                <div className="flex flex-col gap-2 text-xs text-muted-foreground">
                  <p>{t("platform_login_failures_hint")}</p>
                  {isOwner && (
                    <Link to="/admin/audit" className="inline-flex items-center gap-1 font-medium text-primary hover:underline">
                      {t("platform_view_audit")}
                      <ArrowRight className="h-3 w-3" aria-hidden="true" />
                    </Link>
                  )}
                </div>
              </Tile>
            </div>

            {distributed && (
              <section aria-labelledby="platform-nodes" className="space-y-3">
                <h3 id="platform-nodes" className="text-sm font-medium">{t("platform_nodes_title")}</h3>
                {data.nodes.length === 0 ? (
                  <p className="text-sm text-muted-foreground">{t("platform_nodes_empty")}</p>
                ) : (
                  <div className="overflow-x-auto rounded-lg border border-border/60">
                    <NodeTable nodes={data.nodes} />
                  </div>
                )}
              </section>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}

function Tile({ icon: Icon, label, value, muted, warn, children }: {
  icon: LucideIcon;
  label: string;
  value: string;
  muted?: boolean;
  warn?: boolean;
  children: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-3 rounded-lg border border-border/60 bg-muted/10 p-4">
      <div className="flex items-center gap-1.5 text-muted-foreground">
        <Icon className={cn("h-3.5 w-3.5", warn ? "text-amber-500" : "text-primary")} aria-hidden="true" />
        <span className="text-[11px] font-medium">{label}</span>
      </div>
      <div
        className={cn(
          "font-mono font-bold leading-none tabular-nums",
          muted ? "text-sm text-muted-foreground" : "text-xl text-foreground",
          warn && "text-amber-600 dark:text-amber-400",
        )}
      >
        {value}
      </div>
      <div className="mt-auto">{children}</div>
    </div>
  );
}

// The time axis spans the samples held so far, up to the last hour, so a fresh start still fills the width.
function AreaChart({ points, metric, label }: { points: PlatformUsagePoint[]; metric: Metric; label: string }) {
  const { t } = useTranslation("dashboard");
  const gradient = useId();
  if (points.length < 2) {
    return (
      <div className="flex h-16 items-center justify-center rounded-md border border-dashed border-border/60 text-[11px] text-muted-foreground">
        {t("platform_collecting")}
      </div>
    );
  }
  const start = Date.parse(points[0].at);
  const span = Math.max(Date.parse(points[points.length - 1].at) - start, 1);
  const max = Math.max(...points.map((p) => p[metric]), 1) * 1.15;
  const coords = points.map((p) => [((Date.parse(p.at) - start) / span) * 300, 58 - (p[metric] / max) * 52]);
  const line = coords.map(([x, y], i) => `${i === 0 ? "M" : "L"}${x.toFixed(1)},${y.toFixed(1)}`).join(" ");
  const minutes = Math.round(span / 60_000);
  return (
    <div>
      <svg role="img" aria-label={label} viewBox="0 0 300 60" preserveAspectRatio="none" className="h-16 w-full text-primary">
        <defs>
          <linearGradient id={gradient} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="currentColor" stopOpacity="0.28" />
            <stop offset="100%" stopColor="currentColor" stopOpacity="0" />
          </linearGradient>
        </defs>
        <path d={`${line} L300,60 L0,60 Z`} fill={`url(#${gradient})`} />
        <path d={line} fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" vectorEffect="non-scaling-stroke" />
      </svg>
      <div className="mt-1 flex justify-between text-[10px] text-muted-foreground">
        <span>{minutes >= 59 ? t("platform_since_hour") : t("platform_since_minutes", { count: Math.max(minutes, 1) })}</span>
        <span>{t("platform_now")}</span>
      </div>
    </div>
  );
}

// Below md each row is a card: the table elements turn into blocks and the header row is hidden.
function NodeTable({ nodes }: { nodes: NodeUsage[] }) {
  const { t } = useTranslation("dashboard");
  const head = "px-4 py-2.5 font-medium text-muted-foreground";
  const cell = "block md:table-cell md:px-4 md:py-2.5";
  const label = (key: string) => (
    <span className="text-xs text-muted-foreground md:hidden">{t("common:label_prefix", { label: t(key) })}</span>
  );
  return (
    <table className="block w-full text-sm md:table">
      <thead className="hidden md:table-header-group">
        <tr className="border-b border-border bg-muted/30 text-left">
          <th scope="col" className={head}>{t("platform_col_node")}</th>
          <th scope="col" className={head}>{t("platform_col_cpu")}</th>
          <th scope="col" className={head}>{t("platform_col_memory")}</th>
          <th scope="col" className={head}>{t("platform_col_running")}</th>
        </tr>
      </thead>
      <tbody className="block md:table-row-group">
        {nodes.map((node) => (
          <tr key={node.name} className="block space-y-0.5 border-b border-border/50 px-4 py-3 last:border-b-0 md:table-row md:space-y-0 md:p-0">
            <td className={cn(cell, "break-all font-medium")}>{node.name}</td>
            <td className={cell}>
              {label("platform_col_cpu")}
              {t("platform_cpu_value", { cores: cores(node.cpuMilli) })}
            </td>
            <td className={cell}>
              {label("platform_col_memory")}
              {formatBytes(node.memoryBytes)}
            </td>
            <td className={cell}>
              {label("platform_col_running")}
              {node.running}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
