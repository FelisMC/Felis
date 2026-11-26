import { Suspense, useMemo } from "react";
import { Link } from "react-router-dom";
import {
  AlertTriangle,
  ArrowRight,
  CheckCircle,
  LayoutDashboard,
  Loader2,
  RefreshCw,
  ShieldCheck,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Loading, ErrorState, RefreshError } from "@/components/States";
import { AuditTable } from "@/components/AuditTable";
import { PHASE_COLOR, shownPhase } from "@/components/PhaseBadge";
import { PlatformUsageCard } from "@/components/PlatformUsageCard";
import { PageHeader } from "@/components/PageHeader";
import { ErrorBoundary } from "@/components/ErrorBoundary";
import { FleetGrid } from "@/components/FleetGrid";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useConfig, usePolling, type AsyncState } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { PlatformHealthBanner } from "@/components/PlatformHealthBanner";
import { lazyWithReload } from "@/lib/chunk";
import { webglAvailable } from "@/lib/webgl";
import type { Phase, MyServerView, WhitelistImage } from "@/lib/types";

// Retry a failed three.js chunk once, then use the flat grid.
const VoxelFleet = lazyWithReload(() =>
  import("@/components/VoxelFleet").then((m) => ({ default: m.VoxelFleet })),
);

const SERVERS_POLL_MS = 10_000;
const RECENT_AUDIT_COUNT = 10;

export function Dashboard() {
  const cfg = useConfig();
  const { isAdmin, isOwner } = useTier();
  const servers = useAsync(() => api.myServers(), []);
  usePolling(servers.reload, SERVERS_POLL_MS);
  const link = useAsync(() => api.linkStatus(), []);
  const images = useAsync(() => (isAdmin ? api.listImages() : Promise.resolve([])), [isAdmin]);

  const { t } = useTranslation("dashboard");

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
      <PageHeader icon={LayoutDashboard} title={t("title")} subtitle={t("subtitle")} />
      {isOwner && <PlatformHealthBanner />}

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
            servers={list}
            counts={counts}
            link={link}
            images={images.error ? null : images.data}
            isAdmin={isAdmin}
            distributed={cfg.distributed === true}
          />
        </>
      )}
      {isOwner && <RecentAudit />}
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

function LinkCard({ link, staff }: { link: AsyncState<{ linked: boolean }>; staff: boolean }) {
  const { t } = useTranslation("dashboard");

  if (link.error) {
    return (
      <>
        <div role="alert" className="max-w-xl space-y-0.5">
          <div className="flex items-center gap-2 text-xs font-medium text-foreground">
            <AlertTriangle className="h-4 w-4 shrink-0 text-destructive" />
            <span>{t("link_status_failed")}</span>
          </div>
          <p className="text-xs leading-relaxed text-muted-foreground">{humanizeError(link.error)}</p>
        </div>
        <Button
          variant="outline"
          size="sm"
          className="w-full shrink-0 sm:w-auto"
          onClick={link.reload}
          disabled={link.loading}
        >
          <RefreshCw className={link.loading ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
          {t("common:try_again")}
        </Button>
      </>
    );
  }

  if (!link.data) {
    return (
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        <Loader2 className="h-3.5 w-3.5 animate-spin" />
        {t("link_status_checking")}
      </div>
    );
  }

  if (link.data.linked) {
    return (
      <>
        <div className="max-w-xl space-y-0.5">
          <div className="flex items-center gap-2 text-xs font-medium text-foreground">
            <CheckCircle className="h-4 w-4 shrink-0 text-primary" />
            <span>{t("account_linked_title")}</span>
          </div>
          <p className="text-xs leading-relaxed text-muted-foreground">
            {t("account_linked_desc")}
          </p>
        </div>
        <Button variant="outline" size="sm" asChild className="w-full shrink-0 sm:w-auto">
          <Link to="/account">{t("manage_game_character")}</Link>
        </Button>
      </>
    );
  }

  return (
    <>
      <div className="max-w-xl space-y-0.5">
        <div className="flex items-center gap-2 text-xs font-medium text-amber-600 dark:text-amber-400">
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span>{t("account_unlinked_title")}</span>
        </div>
        <p className="text-xs leading-relaxed text-muted-foreground">
          {t(staff ? "staff_unlinked_desc" : "account_unlinked_desc")}
        </p>
      </div>
      <Button variant="default" size="sm" asChild className="w-full shrink-0 sm:w-auto">
        <Link to="/account">
          {t("go_link")} <ArrowRight className="h-3 w-3" />
        </Link>
      </Button>
    </>
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

function FleetView({
  servers,
  counts,
  link,
  images,
  isAdmin,
  distributed,
}: {
  servers: MyServerView[];
  counts: { total: number; running: number; players: number };
  link: AsyncState<{ linked: boolean }>;
  images: WhitelistImage[] | null;
  isAdmin: boolean;
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

  const renderBarSegment = (count: number, color: string) => {
    if (servers.length === 0 || count === 0) return null;
    const widthPct = (count / servers.length) * 100;
    return (
      <div
        className="h-full transition-all"
        style={{ width: `${widthPct}%`, backgroundColor: color }}
      />
    );
  };

  const legendItems = [
    { key: "legend_running", count: statusStats.running, color: PHASE_COLOR.Running, pulse: false },
    { key: "legend_starting", count: statusStats.starting, color: PHASE_COLOR.Starting, pulse: true },
    { key: "legend_stopping", count: statusStats.stopping, color: PHASE_COLOR.Stopping, pulse: true },
    { key: "legend_stopped", count: statusStats.stopped, color: PHASE_COLOR.Stopped, pulse: false },
    { key: "legend_hibernated", count: statusStats.hibernated, color: PHASE_COLOR.Hibernated, pulse: false },
    { key: "legend_failed", count: statusStats.failed, color: PHASE_COLOR.Failed, pulse: false },
    { key: "legend_unknown", count: statusStats.unknown, color: PHASE_COLOR.Unknown, pulse: false },
  ];

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
    <div className="space-y-4">
      <div className="grid grid-cols-1 gap-4 py-1 sm:grid-cols-3">
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

      <div className="grid grid-cols-1 items-stretch gap-4 lg:grid-cols-12">
        <div className="flex flex-col gap-4 lg:col-span-8">
          <Card className="overflow-hidden">
            <CardContent className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <LinkCard link={link} staff={isAdmin} />
            </CardContent>
          </Card>

          {isAdmin ? (
            <>
              <Card>
                <CardHeader className="flex-row items-center justify-between gap-2">
                  <CardTitle>{t("fleet_load")}</CardTitle>
                  <Button variant="outline" size="sm" asChild>
                    <Link to="/servers">
                      {t("go_to_servers_btn")}
                      <ArrowRight className="h-3 w-3" />
                    </Link>
                  </Button>
                </CardHeader>
                <CardContent className="space-y-4">
                  <div className="space-y-1.5">
                    <div className="flex items-center justify-between text-xs">
                      <span className="text-muted-foreground">{t("active_load")}</span>
                      <span className="font-mono font-medium text-foreground tabular-nums">
                        {counts.players} / {statusStats.maxPlayers} {t("stat_players_online")} ({loadPercentage}%)
                      </span>
                    </div>
                    <div className="h-1.5 w-full overflow-hidden rounded-full bg-secondary">
                      <div
                        className="h-full rounded-full bg-primary transition-all"
                        style={{ width: `${loadPercentage}%` }}
                      />
                    </div>
                  </div>

                  <div className="space-y-1.5">
                    <div className="flex items-center justify-between text-xs">
                      <span className="text-muted-foreground">{t("status_distribution")}</span>
                      <span className="font-mono font-medium text-foreground tabular-nums">
                        {t("instances_count", { count: servers.length })}
                      </span>
                    </div>
                    {servers.length > 0 ? (
                      <div className="flex h-2 w-full overflow-hidden rounded-full bg-secondary">
                        {renderBarSegment(statusStats.running, PHASE_COLOR.Running)}
                        {renderBarSegment(statusStats.starting, PHASE_COLOR.Starting)}
                        {renderBarSegment(statusStats.stopping, PHASE_COLOR.Stopping)}
                        {renderBarSegment(statusStats.failed, PHASE_COLOR.Failed)}
                        {renderBarSegment(statusStats.hibernated, PHASE_COLOR.Hibernated)}
                        {renderBarSegment(statusStats.stopped, PHASE_COLOR.Stopped)}
                        {renderBarSegment(statusStats.unknown, PHASE_COLOR.Unknown)}
                      </div>
                    ) : (
                      <div className="rounded bg-muted py-1.5 text-center text-xs text-muted-foreground">
                        {t("no_servers_monitor")}
                      </div>
                    )}
                  </div>

                  <div className="flex items-center gap-2.5 rounded-md border border-border/70 bg-muted px-3 py-2 text-xs text-muted-foreground">
                    <ShieldCheck className="h-4 w-4 shrink-0 text-primary" />
                    <div>
                      <span className="mr-1 font-mono font-semibold text-foreground">{totalImagesCount}</span>
                      {t("images_count")}
                      {t("images_security_note")}
                    </div>
                  </div>
                </CardContent>
              </Card>

              <Card className="flex flex-1 flex-col justify-between">
                <CardHeader>
                  <CardTitle>{t("system_specs")}</CardTitle>
                </CardHeader>
                <CardContent className="flex flex-1 flex-col justify-between">
                  <div className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3">
                    {specs.map((item) => (
                      <div key={item.label} className="space-y-1">
                        <div className="text-[11px] text-muted-foreground">{item.label}</div>
                        <div className={`font-mono text-lg font-semibold leading-none tabular-nums ${item.tone}`}>
                          {item.value}
                        </div>
                      </div>
                    ))}
                  </div>
                </CardContent>
              </Card>
            </>
          ) : (
            <Card className="flex flex-1 flex-col justify-between overflow-hidden">
              <CardHeader>
                <CardTitle>{t("welcome_title")}</CardTitle>
              </CardHeader>
              <CardContent className="flex flex-1 flex-col justify-between gap-4">
                <p className="text-xs leading-relaxed text-muted-foreground">{t("welcome_desc")}</p>

                <div className="space-y-1.5 rounded-md border border-border/75 bg-muted p-3 text-xs text-muted-foreground">
                  <div className="font-medium text-foreground">{t("quick_start_guide")}</div>
                  <ul className="list-inside list-disc space-y-1">
                    <li>{t("quick_start_step1")}</li>
                    <li>{t("quick_start_step2")}</li>
                    <li>{t("quick_start_step3")}</li>
                  </ul>
                </div>

                <div>
                  <Button variant="default" size="sm" asChild className="w-full sm:w-auto">
                    <Link to="/servers">
                      {t("go_to_servers_btn")} <ArrowRight className="h-3.5 w-3.5" />
                    </Link>
                  </Button>
                </div>
              </CardContent>
            </Card>
          )}
        </div>

        <Card className="flex h-full flex-col overflow-hidden lg:col-span-4">
          <CardHeader className="flex-row items-center justify-between gap-2">
            <CardTitle>{t("fleet")}</CardTitle>
            <span className="font-mono text-[11px] text-muted-foreground tabular-nums">
              {t("instances_count", { count: servers.length })}
            </span>
          </CardHeader>
          <CardContent className="flex flex-1 flex-col justify-between p-0">
            <div className="relative min-h-[210px] w-full flex-1 border-b border-border/60 bg-muted">
              <ErrorBoundary fallback={() => <FleetGrid servers={servers} reason="error" />}>
                {webglAvailable() ? (
                  <Suspense fallback={<Loading label={t("loading_scene")} />}>
                    <VoxelFleet servers={servers} />
                  </Suspense>
                ) : (
                  <FleetGrid servers={servers} reason="webgl" />
                )}
              </ErrorBoundary>
            </div>
            <div className="divide-y divide-border/50 text-xs">
              <div className="bg-muted/40 px-3.5 py-2 text-[10.5px] font-medium uppercase tracking-wider text-muted-foreground">
                {t("legend_title")}
              </div>
              {legendItems.map(({ key, count, color, pulse }) => (
                <div key={key} className="flex items-center justify-between px-3.5 py-1.5">
                  <span className="inline-flex items-center gap-2 text-muted-foreground">
                    <span
                      className={`h-[5.5px] w-[5.5px] rounded-full ${pulse && count > 0 ? "animate-pulse" : ""}`}
                      style={{ backgroundColor: color }}
                    />
                    {t(key)}
                  </span>
                  <span className="font-mono text-xs font-medium text-foreground tabular-nums">{count}</span>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      </div>

      {isAdmin && <PlatformUsageCard distributed={distributed} />}
    </div>
  );
}
