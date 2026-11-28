import { ArrowRight, Cpu, MemoryStick, Play, ShieldAlert, type LucideIcon } from "lucide-react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { AreaChart } from "@/components/AreaChart";
import { MetricTile } from "@/components/MetricTile";
import { ErrorState, Loading, RefreshError } from "@/components/States";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { useAsync, usePolling } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import type { NodeUsage } from "@/lib/types";
import { cn } from "@/lib/utils";

const POLL_MS = 15_000;

type Metric = "cpuMilli" | "memoryBytes" | "running";

const cores = (milli: number) => (milli / 1000).toFixed(2);

/** distributed shows the per-node table; on a single node it would repeat the totals. */
export function PlatformUsageCard({ distributed, className }: { distributed: boolean; className?: string }) {
  const { t } = useTranslation("dashboard");
  const { isOwner } = useTier();
  const usage = useAsync(() => api.fleetUsage(), [], { coalesce: true });
  usePolling(usage.reload, POLL_MS);
  const data = usage.data;
  const current = data?.current ?? null;
  const history = data?.history ?? [];

  const metrics: { metric: Metric; icon: LucideIcon; label: string; curve: string; value: string | null }[] = [
    { metric: "cpuMilli", icon: Cpu, label: t("platform_cpu"), curve: t("platform_cpu_curve"), value: current && t("platform_cpu_value", { cores: cores(current.cpuMilli) }) },
    { metric: "memoryBytes", icon: MemoryStick, label: t("platform_memory"), curve: t("platform_memory_curve"), value: current && formatBytes(current.memoryBytes) },
    { metric: "running", icon: Play, label: t("platform_running"), curve: t("platform_running_curve"), value: current && String(current.running) },
  ];

  return (
    <Card className={cn("flex flex-col overflow-hidden", className)}>
      <CardHeader className="flex-row flex-wrap items-center justify-between gap-2">
        <CardTitle>{t("platform_title")}</CardTitle>
        <p className="text-[11px] text-muted-foreground">{t("platform_window")}</p>
      </CardHeader>
      <CardContent className="flex flex-1 flex-col p-0">
        {!data ? (
          <div className="p-4">
            {usage.error ? <ErrorState error={usage.error} onRetry={usage.reload} /> : <Loading />}
          </div>
        ) : (
          <>
            {usage.error != null && <RefreshError error={usage.error} className="m-3.5 mb-0" />}
            <div className="grid flex-1 grid-cols-1 divide-y divide-border/60 sm:grid-cols-2 sm:divide-y-0 xl:grid-cols-4 xl:divide-x">
              {metrics.map(({ metric, icon, label, curve, value }, idx) => (
                <MetricTile
                  key={metric}
                  icon={icon}
                  label={label}
                  value={value ?? t("platform_no_data")}
                  muted={value === null}
                  className={cn(
                    idx % 2 === 1 && "sm:border-l sm:border-border/60 xl:border-l-0",
                    idx >= 2 && "sm:border-t sm:border-border/60 xl:border-t-0",
                  )}
                >
                  <AreaChart points={history.map((p) => ({ at: p.at, value: p[metric] }))} label={curve} />
                </MetricTile>
              ))}
              <MetricTile
                icon={ShieldAlert}
                label={t("platform_login_failures")}
                value={String(data.loginFailures24h)}
                tone={data.loginFailures24h > 0 ? "warn" : undefined}
                className="sm:border-l sm:border-t sm:border-border/60 xl:border-l-0 xl:border-t-0"
              >
                <div className="flex flex-col gap-1.5 text-[11px] text-muted-foreground">
                  <p className="leading-snug">{t("platform_login_failures_hint")}</p>
                  {isOwner && (
                    <Link
                      to="/admin/audit"
                      className="group inline-flex h-6 w-fit items-center gap-1 rounded-md border border-border bg-card px-2 font-medium text-foreground transition-[color,background-color,border-color,transform] duration-150 ease-out hover:border-muted-foreground/30 hover:bg-accent/70 active:scale-[0.98]"
                    >
                      {t("platform_view_audit")}
                      <ArrowRight className="h-3 w-3 transition-transform duration-150 ease-out group-hover:translate-x-0.5" aria-hidden="true" />
                    </Link>
                  )}
                </div>
              </MetricTile>
            </div>

            {distributed && (
              <section aria-labelledby="platform-nodes" className="space-y-2 border-t border-border/60 p-3.5">
                <h3 id="platform-nodes" className="text-xs font-medium text-muted-foreground">{t("platform_nodes_title")}</h3>
                {data.nodes.length === 0 ? (
                  <p className="text-xs text-muted-foreground">{t("platform_nodes_empty")}</p>
                ) : (
                  <div className="overflow-x-auto rounded-md border border-border/75">
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

function NodeTable({ nodes }: { nodes: NodeUsage[] }) {
  const { t } = useTranslation("dashboard");
  const head = "px-3.5 py-2 text-[11px] font-medium text-muted-foreground";
  const cell = "block md:table-cell md:px-3.5 md:py-2";
  const label = (key: string) => (
    <span className="text-[11px] text-muted-foreground md:hidden">{t("common:label_prefix", { label: t(key) })}</span>
  );
  return (
    <table className="block w-full text-xs md:table">
      <thead className="hidden md:table-header-group">
        <tr className="border-b border-border/60 bg-muted text-left">
          <th scope="col" className={head}>{t("platform_col_node")}</th>
          <th scope="col" className={head}>{t("platform_col_cpu")}</th>
          <th scope="col" className={head}>{t("platform_col_memory")}</th>
          <th scope="col" className={head}>{t("platform_col_running")}</th>
        </tr>
      </thead>
      <tbody className="block md:table-row-group">
        {nodes.map((node) => (
          <tr key={node.name} className="block space-y-0.5 border-b border-border/50 px-3.5 py-2 last:border-b-0 md:table-row md:space-y-0 md:p-0">
            <td className={cn(cell, "break-all font-medium")}>{node.name}</td>
            <td className={cn(cell, "font-mono text-xs tabular-nums")}>
              {label("platform_col_cpu")}
              {t("platform_cpu_value", { cores: cores(node.cpuMilli) })}
            </td>
            <td className={cn(cell, "font-mono text-xs tabular-nums")}>
              {label("platform_col_memory")}
              {formatBytes(node.memoryBytes)}
            </td>
            <td className={cn(cell, "font-mono text-xs tabular-nums")}>
              {label("platform_col_running")}
              {node.running}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
