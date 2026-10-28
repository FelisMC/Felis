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
export function PlatformUsageCard({ distributed }: { distributed: boolean }) {
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
                <MetricTile key={metric} icon={icon} label={label} value={value ?? t("platform_no_data")} muted={value === null}>
                  <AreaChart points={history.map((p) => ({ at: p.at, value: p[metric] }))} label={curve} />
                </MetricTile>
              ))}
              <MetricTile
                icon={ShieldAlert}
                label={t("platform_login_failures")}
                value={String(data.loginFailures24h)}
                tone={data.loginFailures24h > 0 ? "warn" : undefined}
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
              </MetricTile>
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
