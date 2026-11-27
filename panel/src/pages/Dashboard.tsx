import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  AlertTriangle,
  ArrowRight,
  Loader2,
  RefreshCw,
  Terminal,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Loading, ErrorState, RefreshError } from "@/components/States";
import { AuditTable } from "@/components/AuditTable";
import { PHASE_COLOR, PhaseBadge, shownPhase, startFailure } from "@/components/PhaseBadge";
import { PlatformUsageCard } from "@/components/PlatformUsageCard";
import { PageHeader } from "@/components/PageHeader";
import { FleetGrid } from "@/components/FleetGrid";
import { api, humanizeError } from "@/lib/api";
import type { RuntimeConfig } from "@/lib/config";
import { useAsync, useConfig, usePolling, type AsyncState } from "@/lib/hooks";
import { cpuUsage, memoryUsage } from "@/lib/resourceUsage";
import { useTier } from "@/lib/tier";
import { PlatformHealthBanner } from "@/components/PlatformHealthBanner";
import { cn } from "@/lib/utils";
import type { Phase, MyServerView, WhitelistImage } from "@/lib/types";

const SERVERS_POLL_MS = 10_000;
const RECENT_AUDIT_COUNT = 10;
const FOCUS_LIMIT = 6;

export function Dashboard() {
  const cfg = useConfig();
  const { isAdmin, isOwner } = useTier();
  const servers = useAsync(() => api.myServers(), []);
  usePolling(servers.reload, SERVERS_POLL_MS);
  const link = useAsync(() => api.linkStatus(), []);
  const images = useAsync(() => (isAdmin ? api.listImages() : Promise.resolve([])), [isAdmin]);

  const { t } = useTranslation(["dashboard", "admin"]);

  const list = useMemo(
    () =>
      (servers.data ?? []).map((s) => ({
        ...s,
        phase: shownPhase({ phase: s.phase ?? "Unknown", desiredState: s.desiredState }),
      })),
    [servers.data],
  );

  const counts = useMemo(() => {
    const by = (p: Phase) => list.filter((s) => s.phase === p).length;
    return {
      total: list.length,
      running: by("Running"),
      players: list.reduce((n, s) => n + (s.playersOnline ?? 0), 0),
    };
  }, [list]);

  return (
    <div className="space-y-4">
      <PageHeader
        title={t("title")}
        subtitle={isOwner ? <PlatformHealthBanner inline /> : t("subtitle")}
        actions={
          <>
            {isOwner && (
              <Button variant="outline" size="sm" asChild>
                <Link to="/admin/updates">{t("admin:health_open")}</Link>
              </Button>
            )}
            <Button size="sm" asChild>
              <Link to="/servers">{t("go_to_servers_btn")}</Link>
            </Button>
          </>
        }
      />

      {servers.loading && !servers.data ? (
        <Loading />
      ) : servers.error && !servers.data ? (
        <ErrorState error={servers.error} onRetry={servers.reload} />
      ) : !cfg ? (
        <Loading />
      ) : (
        <>
          {servers.error && <RefreshError error={servers.error} />}
          <FleetView
            cfg={cfg}
            servers={list}
            counts={counts}
            link={link}
            images={images.error ? null : images.data}
            isAdmin={isAdmin}
            isOwner={isOwner}
            distributed={cfg.distributed === true}
          />
        </>
      )}
    </div>
  );
}

function RecentAudit() {
  const { t } = useTranslation("dashboard");
  const audit = useAsync(() => api.listAudit({ limit: RECENT_AUDIT_COUNT }), []);

  return (
    <Card className="overflow-hidden">
      <CardHeader className="flex-row items-center justify-between gap-3">
        <CardTitle>{t("audit_title")}</CardTitle>
        <Button variant="outline" size="sm" asChild>
          <Link to="/admin/audit">
            {t("audit_view_all")}
            <ArrowRight className="h-3 w-3" />
          </Link>
        </Button>
      </CardHeader>
      <CardContent className="p-0">
        {audit.data ? (
          audit.data.entries.length > 0 ? (
            <AuditTable entries={audit.data.entries} />
          ) : (
            <p className="p-4 text-xs text-muted-foreground">{t("audit_empty")}</p>
          )
        ) : audit.error ? (
          <ErrorState error={audit.error} onRetry={audit.reload} />
        ) : (
          <Loading />
        )}
      </CardContent>
    </Card>
  );
}

function LinkRow({ link, staff }: { link: AsyncState<{ linked: boolean }>; staff: boolean }) {
  const { t } = useTranslation("dashboard");

  if (link.error) {
    return (
      <div className="space-y-2 px-3.5 py-2.5 text-xs">
        <div role="alert" className="space-y-0.5">
          <div className="flex items-center gap-1.5 font-medium text-foreground">
            <AlertTriangle className="h-3.5 w-3.5 shrink-0 text-destructive" />
            <span>{t("link_status_failed")}</span>
          </div>
          <p className="text-[11px] leading-relaxed text-muted-foreground">{humanizeError(link.error)}</p>
        </div>
        <Button variant="outline" size="sm" className="w-full" onClick={link.reload} disabled={link.loading}>
          <RefreshCw className={link.loading ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
          {t("common:try_again")}
        </Button>
      </div>
    );
  }

  if (!link.data) {
    return (
      <div className="flex items-center gap-2 px-3.5 py-2.5 text-xs text-muted-foreground">
        <Loader2 className="h-3.5 w-3.5 animate-spin" />
        {t("link_status_checking")}
      </div>
    );
  }

  if (link.data.linked) {
    return (
      <div className="flex items-center justify-between gap-2 px-3.5 py-2 text-xs">
        <span className="inline-flex min-w-0 items-center gap-1.5 text-foreground">
          <span aria-hidden="true" className="h-[5.5px] w-[5.5px] shrink-0 rounded-full bg-primary" />
          <span className="truncate">{t("account_linked_title")}</span>
        </span>
        <Button variant="outline" size="sm" asChild className="h-6 shrink-0 px-2 text-[11px]">
          <Link to="/account">{t("manage_game_character")}</Link>
        </Button>
      </div>
    );
  }

  return (
    <div className="space-y-2 px-3.5 py-2.5 text-xs">
      <div className="space-y-0.5">
        <div className="flex items-center gap-1.5 font-medium text-amber-600 dark:text-amber-400">
          <span aria-hidden="true" className="h-[5.5px] w-[5.5px] shrink-0 rounded-full bg-amber-500" />
          <span>{t("account_unlinked_title")}</span>
        </div>
        <p className="text-[11px] leading-relaxed text-muted-foreground">
          {t(staff ? "staff_unlinked_desc" : "account_unlinked_desc")}
        </p>
      </div>
      <Button variant="default" size="sm" asChild className="w-full">
        <Link to="/account">
          {t("go_link")} <ArrowRight className="h-3 w-3" />
        </Link>
      </Button>
    </div>
  );
}

function SummaryMetric({
  label,
  value,
  suffix,
  pct,
  accent = false,
}: {
  label: string;
  value: React.ReactNode;
  suffix?: React.ReactNode;
  pct: number;
  accent?: boolean;
}) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-[11px] text-muted-foreground">{label}</span>
      <div className="flex items-baseline gap-1 font-mono text-[19px] font-semibold leading-tight tracking-tight text-foreground tabular-nums">
        <span>{value}</span>
        {suffix && <span className="text-xs font-normal text-muted-foreground">{suffix}</span>}
      </div>
      <div className="mt-1 h-[2px] w-full overflow-hidden rounded-full bg-secondary">
        <div
          className={accent ? "h-full rounded-full bg-primary transition-all" : "h-full rounded-full bg-foreground/35 transition-all"}
          style={{ width: `${Math.min(100, Math.max(0, pct))}%` }}
        />
      </div>
    </div>
  );
}

type FocusTab = "attention" | "running" | "all";

function isAttention(s: MyServerView): boolean {
  return s.phase === "Failed" || s.phase === "Starting" || s.phase === "Stopping";
}

function FleetFocusCard({
  servers,
  cfg,
  isAdmin,
}: {
  servers: MyServerView[];
  cfg: RuntimeConfig;
  isAdmin: boolean;
}) {
  const { t } = useTranslation(["dashboard", "ops", "servers"]);
  const attention = useMemo(() => servers.filter(isAttention), [servers]);
  const running = useMemo(() => servers.filter((s) => s.phase === "Running"), [servers]);
  const [picked, setPicked] = useState<FocusTab | null>(null);
  const tab: FocusTab = picked ?? (attention.length > 0 ? "attention" : running.length > 0 ? "running" : "all");
  const rows = (tab === "attention" ? attention : tab === "running" ? running : servers).slice(0, FOCUS_LIMIT);

  const tabs: { key: FocusTab; label: string; count: number }[] = [
    { key: "attention", label: t("ops:fleet_stat_failed"), count: attention.length },
    { key: "running", label: t("dashboard:legend_running"), count: running.length },
    { key: "all", label: t("ops:fleet_filter_all"), count: servers.length },
  ];

  return (
    <Card className="overflow-hidden">
      <CardHeader className="flex-row flex-wrap items-center justify-between gap-2 py-2">
        <div className="flex items-center gap-4">
          {tabs.map((item) => {
            const active = tab === item.key;
            return (
              <button
                key={item.key}
                type="button"
                onClick={() => setPicked(item.key)}
                className={cn(
                  "relative inline-flex items-center gap-1.5 pb-1 pt-0.5 text-xs transition-colors",
                  active ? "font-medium text-foreground" : "text-muted-foreground hover:text-foreground",
                )}
              >
                <span>{item.label}</span>
                <span className="font-mono text-[11px] text-muted-foreground tabular-nums">{item.count}</span>
                <span
                  aria-hidden="true"
                  className={cn("absolute inset-x-0 -bottom-2 h-[1.5px]", active ? "bg-primary" : "bg-transparent")}
                />
              </button>
            );
          })}
        </div>
      </CardHeader>
      <CardContent className="p-0">
        {rows.length === 0 ? (
          <p className="px-3.5 py-6 text-center text-xs text-muted-foreground">
            {servers.length === 0 ? t("dashboard:no_servers_hint") : t("ops:fleet_no_match_title")}
          </p>
        ) : (
          <>
            <div className="hidden grid-cols-[minmax(0,1fr)_8.25rem_4.25rem_4.75rem] items-center gap-3 border-b border-border/60 bg-muted px-3.5 py-2 text-[11px] text-muted-foreground sm:grid xl:grid-cols-[minmax(0,1fr)_8.25rem_4.25rem_7.5rem_4.75rem]">
              <span>{t("ops:fleet_col_server")}</span>
              <span>{t("ops:fleet_col_status")}</span>
              <span>{t("ops:fleet_col_players")}</span>
              <span className="hidden xl:block">{t("servers:usage_cpu")} / {t("servers:usage_memory")}</span>
              <span className="text-right">{t("ops:fleet_col_actions")}</span>
            </div>
            <ul className="divide-y divide-border/50 text-xs">
              {rows.map((s) => {
                const phase = s.phase ?? "Unknown";
                const host = s.subdomain && cfg.rootDomain ? `${s.subdomain}.${cfg.rootDomain}` : s.name;
                const address = s.subdomain && cfg.rootDomain && cfg.gamePort && cfg.gamePort !== 25565 ? `${host}:${cfg.gamePort}` : host;
                const live = phase === "Running" || phase === "Starting" || phase === "Stopping";
                const cpu = cpuUsage(s.cpuMilli, s.cpu);
                const mem = memoryUsage(s.memoryBytes, s.memory);
                const resource = cpu !== "—" || mem !== "—" ? `${cpu} · ${mem}` : "—";
                const target = s.owned || isAdmin ? `/servers/${s.name}` : "/servers";
                return (
                  <li
                    key={s.name}
                    className="grid grid-cols-1 items-center gap-2 px-3.5 py-2.5 transition-colors hover:bg-accent/40 sm:grid-cols-[minmax(0,1fr)_8.25rem_4.25rem_4.75rem] sm:gap-3 xl:grid-cols-[minmax(0,1fr)_8.25rem_4.25rem_7.5rem_4.75rem]"
                  >
                    <div className="min-w-0">
                      <Link to={target} className="block truncate font-medium text-foreground transition-colors hover:text-primary hover:underline">
                        {s.displayName || s.name}
                      </Link>
                      <div className="truncate font-mono text-[10.5px] text-muted-foreground">{address}</div>
                    </div>
                    <div>
                      <PhaseBadge phase={phase} failure={startFailure(s)} autoRestarts={s.autoRestarts} />
                    </div>
                    <div className="font-mono text-muted-foreground tabular-nums">
                      {live ? `${s.playersOnline} / ${s.playersMax}` : "—"}
                    </div>
                    <div className="hidden truncate font-mono text-[11px] text-muted-foreground tabular-nums xl:block" title={resource}>
                      {resource}
                    </div>
                    <div className="flex justify-end">
                      {s.owned || isAdmin ? (
                        <Button variant="outline" size="sm" asChild className="h-6 px-2 text-[11px]">
                          <Link to={`/servers/${s.name}`}>
                            <Terminal />
                            {t("servers:console")}
                          </Link>
                        </Button>
                      ) : (
                        <Button variant="outline" size="sm" asChild className="h-6 px-2 text-[11px]">
                          <Link to="/servers">{t("dashboard:manage")}</Link>
                        </Button>
                      )}
                    </div>
                  </li>
                );
              })}
            </ul>
          </>
        )}
      </CardContent>
    </Card>
  );
}

function FleetView({
  cfg,
  servers,
  counts,
  link,
  images,
  isAdmin,
  isOwner,
  distributed,
}: {
  cfg: RuntimeConfig;
  servers: MyServerView[];
  counts: { total: number; running: number; players: number };
  link: AsyncState<{ linked: boolean }>;
  images: WhitelistImage[] | null;
  isAdmin: boolean;
  isOwner: boolean;
  distributed: boolean;
}) {
  const { t } = useTranslation("dashboard");

  const statusStats = useMemo(() => {
    let running = 0;
    let starting = 0;
    let stopping = 0;
    let stopped = 0;
    let hibernated = 0;
    let failed = 0;
    let unknown = 0;
    let maxPlayers = 0;

    servers.forEach((s) => {
      if (s.phase === "Running") running++;
      else if (s.phase === "Starting") starting++;
      else if (s.phase === "Stopping") stopping++;
      else if (s.phase === "Stopped") stopped++;
      else if (s.phase === "Hibernated" || s.phase === "Hibernating") hibernated++;
      else if (s.phase === "Failed") failed++;
      else unknown++;

      maxPlayers += s.playersMax ?? 0;
    });

    return {
      running,
      starting,
      stopping,
      stopped,
      hibernated,
      failed,
      unknown,
      maxPlayers,
    };
  }, [servers]);

  const loadPercentage = statusStats.maxPlayers > 0
    ? Math.round((counts.players / statusStats.maxPlayers) * 100)
    : 0;

  const imageCount = (keep: (img: WhitelistImage) => boolean) =>
    images ? images.filter(keep).length : "—";
  const totalImagesCount = imageCount(() => true);
  const builtImagesCount = imageCount((img) => img.source === "built" || !img.source);
  const externalImagesCount = imageCount((img) => img.source === "external");

  const ownedServersCount = useMemo(
    () => servers.filter((s) => s.owned).length,
    [servers],
  );
  const claimableServersCount = useMemo(
    () => servers.filter((s) => s.claimable).length,
    [servers],
  );

  const runningRatio = useMemo(
    () => (servers.length > 0 ? Math.round((statusStats.running / servers.length) * 100) : 0),
    [servers, statusStats.running],
  );
  const stoppedRatio = useMemo(
    () => (servers.length > 0 ? Math.round((statusStats.stopped / servers.length) * 100) : 0),
    [servers, statusStats.stopped],
  );
  const failedRatio = useMemo(
    () => (servers.length > 0 ? Math.round((statusStats.failed / servers.length) * 100) : 0),
    [servers, statusStats.failed],
  );

  const legendItems = [
    { key: "legend_running", count: statusStats.running, ratio: runningRatio, color: PHASE_COLOR.Running, pulse: false, always: true },
    { key: "legend_starting", count: statusStats.starting, ratio: null, color: PHASE_COLOR.Starting, pulse: true, always: true },
    { key: "legend_stopping", count: statusStats.stopping, ratio: null, color: PHASE_COLOR.Stopping, pulse: true, always: false },
    { key: "legend_failed", count: statusStats.failed, ratio: failedRatio, color: PHASE_COLOR.Failed, pulse: false, always: true },
    { key: "legend_hibernated", count: statusStats.hibernated, ratio: null, color: PHASE_COLOR.Hibernated, pulse: false, always: false },
    { key: "legend_stopped", count: statusStats.stopped, ratio: stoppedRatio, color: PHASE_COLOR.Stopped, pulse: false, always: true },
    { key: "legend_unknown", count: statusStats.unknown, ratio: null, color: PHASE_COLOR.Unknown, pulse: false, always: false },
  ].filter((item) => item.always || item.count > 0);

  // Keep space-y-1 with numeric value as lastElementChild for Dashboard.test.tsx specValue().
  const specs = [
    { label: t("spec_images_total"), value: totalImagesCount, tone: "text-foreground" },
    { label: t("spec_images_built"), value: builtImagesCount, tone: "text-foreground" },
    { label: t("spec_images_external"), value: externalImagesCount, tone: "text-foreground" },
    { label: t("spec_servers_total"), value: counts.total, tone: "text-foreground" },
    { label: t("spec_servers_owned"), value: ownedServersCount, tone: "text-foreground" },
    { label: t("spec_servers_claimable"), value: claimableServersCount, tone: "text-foreground" },
    { label: t("spec_running_ratio"), value: `${runningRatio}%`, tone: "text-primary" },
    { label: t("spec_stopped_ratio"), value: `${stoppedRatio}%`, tone: "text-muted-foreground" },
    { label: t("spec_failed_ratio"), value: `${failedRatio}%`, tone: statusStats.failed > 0 ? "text-destructive" : "text-muted-foreground" },
  ];

  return (
    <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-[minmax(0,1fr)_264px] xl:grid-cols-[minmax(0,1fr)_284px]">
      <div className="min-w-0 space-y-4">
        <div className="grid grid-cols-1 gap-4 py-0.5 sm:grid-cols-3">
          <SummaryMetric label={t("stat_servers")} value={counts.total} pct={counts.total > 0 ? 100 : 0} />
          <SummaryMetric
            label={t("stat_running")}
            value={counts.running}
            suffix={`/ ${counts.total}`}
            pct={runningRatio}
            accent
          />
          <SummaryMetric
            label={t("stat_players_online")}
            value={counts.players}
            suffix={statusStats.maxPlayers > 0 ? `/ ${statusStats.maxPlayers}` : undefined}
            pct={loadPercentage}
            accent
          />
        </div>

        <FleetFocusCard servers={servers} cfg={cfg} isAdmin={isAdmin} />

        {isAdmin && <PlatformUsageCard distributed={distributed} />}
        {isOwner && <RecentAudit />}
      </div>

      <div className="space-y-4">
        <Card className="overflow-hidden">
          <CardHeader className="flex-row items-center justify-between gap-2">
            <CardTitle>{t("fleet")}</CardTitle>
            <span className="font-mono text-[11px] text-muted-foreground tabular-nums">
              {t("instances_count", { count: servers.length })}
            </span>
          </CardHeader>
          <CardContent className="p-0">
            <div className="border-b border-border/60 bg-muted/50">
              <FleetGrid servers={servers} />
            </div>
            <div className="divide-y divide-border/50 text-xs">
              {legendItems.map(({ key, count, ratio, color, pulse }) => (
                <div key={key} className="flex items-center justify-between px-3.5 py-2">
                  <span className="inline-flex items-center gap-2 text-muted-foreground">
                    <span
                      className={`h-[5.5px] w-[5.5px] rounded-full ${pulse && count > 0 ? "animate-pulse" : ""}`}
                      style={{ backgroundColor: color }}
                    />
                    {t(key)}
                  </span>
                  <span className="font-mono text-foreground tabular-nums">
                    {count}
                    {ratio !== null && servers.length > 0 && (
                      <span className="ml-1.5 text-[10.5px] text-muted-foreground">{ratio}%</span>
                    )}
                  </span>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>

        <Card className="overflow-hidden">
          <CardHeader>
            <CardTitle>{t("system_specs")}</CardTitle>
          </CardHeader>
          <CardContent className="divide-y divide-border/50 p-0">
            <LinkRow link={link} staff={isAdmin} />
            {isAdmin &&
              specs.map((item) => (
                <div
                  key={item.label}
                  className="flex items-center justify-between gap-2 space-y-1 px-3.5 py-2 text-xs [&>*]:!mt-0"
                >
                  <span className="text-muted-foreground">{item.label}</span>
                  <span className={`font-mono font-medium tabular-nums ${item.tone}`}>{item.value}</span>
                </div>
              ))}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
