import { Activity } from "lucide-react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { UsageChart } from "@/components/ResourceUsage";
import { ErrorState, Loading, RefreshError } from "@/components/States";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { useAsync, usePolling } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import type { NodeUsage } from "@/lib/types";
import { cn } from "@/lib/utils";

const POLL_MS = 15_000;

const cores = (milli: number) => (milli / 1000).toFixed(2);

export function PlatformUsageCard() {
  const { t } = useTranslation("dashboard");
  const { isOwner } = useTier();
  const usage = useAsync(() => api.fleetUsage(), [], { coalesce: true });
  usePolling(usage.reload, POLL_MS);
  const data = usage.data;
  const current = data?.current ?? null;

  const metrics = [
    { metric: "cpuMilli", label: t("platform_cpu"), curve: t("platform_cpu_curve"), value: current && t("platform_cpu_value", { cores: cores(current.cpuMilli) }) },
    { metric: "memoryBytes", label: t("platform_memory"), curve: t("platform_memory_curve"), value: current && formatBytes(current.memoryBytes) },
    { metric: "running", label: t("platform_running"), curve: t("platform_running_curve"), value: current && String(current.running) },
  ] as const;

  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-center justify-between gap-2">
        <CardTitle className="flex items-center gap-2">
          <Activity className="text-muted-foreground" aria-hidden="true" />
          {t("platform_title")}
        </CardTitle>
        <p className="text-xs text-muted-foreground">{t("platform_window")}</p>
      </CardHeader>
      <CardContent className="space-y-5">
        {!data ? (
          usage.error ? <ErrorState error={usage.error} onRetry={usage.reload} /> : <Loading />
        ) : (
          <>
            {usage.error != null && <RefreshError error={usage.error} />}
            <dl className="grid gap-4 sm:grid-cols-3">
              {metrics.map(({ metric, label, curve, value }) => (
                <div key={metric} className="min-w-0 space-y-2">
                  <div className="flex items-baseline justify-between gap-2 text-sm">
                    <dt>{label}</dt>
                    <dd className="tabular-nums">{value ?? t("platform_no_data")}</dd>
                  </div>
                  <UsageChart history={data.history} metric={metric} label={curve} limit={0} />
                </div>
              ))}
            </dl>

            <section aria-labelledby="platform-nodes" className="space-y-2">
              <h3 id="platform-nodes" className="text-sm font-medium">{t("platform_nodes_title")}</h3>
              {data.nodes.length === 0 ? (
                <p className="text-sm text-muted-foreground">{t("platform_nodes_empty")}</p>
              ) : (
                <div className="overflow-x-auto rounded-md border border-border">
                  <NodeTable nodes={data.nodes} />
                </div>
              )}
            </section>

            <section
              aria-labelledby="platform-login-failures"
              className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-border p-3"
            >
              <div>
                <h3 id="platform-login-failures" className="text-xs font-medium text-muted-foreground">{t("platform_login_failures")}</h3>
                <p className={cn("mt-1 font-mono text-2xl font-bold", data.loginFailures24h > 0 && "text-amber-600 dark:text-amber-400")}>
                  {data.loginFailures24h}
                </p>
              </div>
              {isOwner && (
                <Link to="/admin/audit" className="text-xs font-medium text-primary hover:underline">
                  {t("platform_view_audit")}
                </Link>
              )}
            </section>
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
