import { Suspense, useMemo } from "react";
import { Link } from "react-router-dom";
import {
  Activity,
  Server,
  Play,
  CheckCircle,
  AlertTriangle,
  ArrowRight,
  Cpu,
  ShieldCheck,
  Database,
  Box,
  Hand,
  LayoutDashboard,
  Loader2,
  RefreshCw,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Loading, ErrorState } from "@/components/States";
import { StatCard } from "@/components/StatCard";
import { PageHeader } from "@/components/PageHeader";
import { ErrorBoundary } from "@/components/ErrorBoundary";
import { FleetGrid } from "@/components/FleetGrid";
import { api, humanizeError } from "@/lib/api";
import { useAsync, useConfig, type AsyncState } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { lazyWithReload } from "@/lib/chunk";
import { webglAvailable } from "@/lib/webgl";
import type { Phase, MyServerView, WhitelistImage } from "@/lib/types";

// three.js is heavy and only the Dashboard renders it — split it into its own
// async chunk so the rest of the panel doesn't pay for it on first load.
// A failed chunk reloads the tab once (new deploy); after that the card falls
// back to the flat grid instead of taking the page down.
const VoxelFleet = lazyWithReload(() =>
  import("@/components/VoxelFleet").then((m) => ({ default: m.VoxelFleet })),
);

export function Dashboard() {
  const cfg = useConfig();
  const { isAdmin } = useTier();
  const servers = useAsync(() => api.myServers(), []);
  const link = useAsync(() => api.linkStatus(), []);
  // Only admins may list the image whitelist (a user would get a 403).
  const images = useAsync(() => (isAdmin ? api.listImages() : Promise.resolve([])), [isAdmin]);

  const { t } = useTranslation("dashboard");

  const counts = useMemo(() => {
    const list = servers.data ?? [];
    const by = (p: Phase) => list.filter((s) => s.phase === p).length;
    return {
      total: list.length,
      running: by("Running"),
      players: list.reduce((n, s) => n + (s.playersOnline ?? 0), 0),
    };
  }, [servers.data]);

  // The page stands on the server list alone. Link status and the image
  // counts are side cards: when they fail only their card degrades, and each
  // offers its own retry.
  return (
    <>
      <PageHeader icon={LayoutDashboard} title={t("title")} subtitle={t("subtitle")} />

      {servers.loading && !servers.data ? (
        <Loading />
      ) : servers.error ? (
        <ErrorState error={servers.error} onRetry={servers.reload} />
      ) : !cfg ? (
        <Loading />
      ) : (
        <FleetView
          servers={servers.data ?? []}
          counts={counts}
          link={link}
          images={images.error ? null : images.data}
          isAdmin={isAdmin}
        />
      )}
    </>
  );
}

function LinkCard({ link }: { link: AsyncState<{ linked: boolean }> }) {
  const { t } = useTranslation("dashboard");

  if (link.error) {
    return (
      <>
        <div role="alert" className="space-y-1 max-w-xl">
          <div className="flex items-center gap-2 text-muted-foreground font-semibold text-sm">
            <AlertTriangle className="h-5 w-5 shrink-0" />
            <span>{t("link_status_failed")}</span>
          </div>
          <p className="text-xs text-muted-foreground leading-relaxed">{humanizeError(link.error)}</p>
        </div>
        <Button
          variant="outline"
          size="sm"
          className="shrink-0 w-full sm:w-auto text-xs gap-1.5"
          onClick={link.reload}
          disabled={link.loading}
        >
          <RefreshCw className={link.loading ? "h-3 w-3 animate-spin" : "h-3 w-3"} />
          {t("common:try_again")}
        </Button>
      </>
    );
  }

  if (!link.data) {
    return (
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        {t("link_status_checking")}
      </div>
    );
  }

  if (link.data.linked) {
    return (
      <>
        <div className="space-y-1 max-w-xl">
          <div className="flex items-center gap-2 text-green-600 dark:text-green-400 font-semibold text-sm">
            <CheckCircle className="h-5 w-5 shrink-0" />
            <span>{t("account_linked_title")}</span>
          </div>
          <p className="text-xs text-muted-foreground leading-relaxed">
            {t("account_linked_desc")}
          </p>
        </div>
        <Link to="/account" className="shrink-0 w-full sm:w-auto">
          <Button variant="outline" size="sm" className="w-full text-xs">
            {t("manage_game_character")}
          </Button>
        </Link>
      </>
    );
  }

  return (
    <>
      <div className="space-y-1 max-w-xl">
        <div className="flex items-center gap-2 text-amber-600 dark:text-amber-500 font-semibold text-sm">
          <AlertTriangle className="h-5 w-5 shrink-0 animate-bounce" />
          <span>{t("account_unlinked_title")}</span>
        </div>
        <p className="text-xs text-muted-foreground leading-relaxed">
          {t("account_unlinked_desc")}
        </p>
      </div>
      <Link to="/account" className="shrink-0 w-full sm:w-auto">
        <Button variant="default" size="sm" className="w-full text-xs">
          {t("go_link")} <ArrowRight className="h-3 w-3 ml-1" />
        </Button>
      </Link>
    </>
  );
}

function FleetView({
  servers,
  counts,
  link,
  images,
  isAdmin,
}: {
  servers: MyServerView[];
  counts: { total: number; running: number; players: number };
  link: AsyncState<{ linked: boolean }>;
  /** null while the whitelist loads or when it cannot be read: its counts show a dash. */
  images: WhitelistImage[] | null;
  isAdmin: boolean;
}) {
  const { t } = useTranslation("dashboard");

  // 1. 统计各状态 of 服务器数量
  const statusStats = useMemo(() => {
    let running = 0;
    let starting = 0;
    let stopping = 0;
    let stopped = 0;
    let failed = 0;
    let unknown = 0;
    let maxPlayers = 0;

    servers.forEach((s) => {
      if (s.phase === "Running") running++;
      else if (s.phase === "Starting") starting++;
      else if (s.phase === "Stopping") stopping++;
      else if (s.phase === "Stopped") stopped++;
      else if (s.phase === "Failed") failed++;
      else unknown++;

      maxPlayers += s.playersMax ?? 0;
    });

    return {
      running,
      starting,
      stopping,
      stopped,
      failed,
      unknown,
      maxPlayers,
    };
  }, [servers]);

  const loadPercentage = statusStats.maxPlayers > 0 
    ? Math.round((counts.players / statusStats.maxPlayers) * 100) 
    : 0;

  // 2. 自建与外部镜像真实统计
  const imageCount = (keep: (img: WhitelistImage) => boolean) =>
    images ? images.filter(keep).length : "—";
  const totalImagesCount = imageCount(() => true);
  const builtImagesCount = imageCount((img) => img.source === "built" || !img.source);
  const externalImagesCount = imageCount((img) => img.source === "external");

  // 3. 我拥有及可认领服务器真实统计
  const ownedServersCount = useMemo(() => 
    servers.filter((s) => s.owned).length, 
    [servers]
  );
  const claimableServersCount = useMemo(() => 
    servers.filter((s) => s.claimable).length, 
    [servers]
  );

  // 4. 各种真实状态比率统计
  const runningRatio = useMemo(() => 
    servers.length > 0 ? Math.round((statusStats.running / servers.length) * 100) : 0,
    [servers, statusStats.running]
  );
  const stoppedRatio = useMemo(() => 
    servers.length > 0 ? Math.round((statusStats.stopped / servers.length) * 100) : 0,
    [servers, statusStats.stopped]
  );
  const failedRatio = useMemo(() => 
    servers.length > 0 ? Math.round((statusStats.failed / servers.length) * 100) : 0,
    [servers, statusStats.failed]
  );

  // 辅助渲染百分比进度条段
  const renderBarSegment = (count: number, colorClass: string) => {
    if (servers.length === 0 || count === 0) return null;
    const widthPct = (count / servers.length) * 100;
    return (
      <div
        className={`${colorClass} h-full transition-all`}
        style={{ width: `${widthPct}%` }}
      />
    );
  };

  return (
    <div className="space-y-6">
      {/* 顶部 Stat */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <StatCard icon={Server} label={t("stat_servers")} value={counts.total} accentClass="text-primary bg-primary/15" />
        <StatCard icon={Play} label={t("stat_running")} value={counts.running} accentClass="text-primary bg-primary/15" />
        <StatCard icon={Activity} label={t("stat_players_online")} value={counts.players} accentClass="text-primary bg-primary/15" />
      </div>

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-12 items-stretch">
        {/* 左侧：账号状态、健康度及负载监控 (lg:col-span-8 h-full 捕获拉伸高度) */}
        <div className="lg:col-span-8 flex flex-col gap-6 h-full">
          {/* 1. 游戏角色绑定 Banner */}
          <Card className="overflow-hidden">
            <CardContent className="p-5 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4">
              <LinkCard link={link} />
            </CardContent>
          </Card>

          {/* 2. 管理员展示集群负载健康与镜像参数看板，普通用户展示极简引导 */}
          {isAdmin ? (
            <>
              {/* 集群负载与健康卡片 */}
              <Card>
                <CardHeader className="pb-3 pt-5">
                  <CardTitle className="text-base">{t("fleet_load")}</CardTitle>
                </CardHeader>
                <CardContent className="space-y-5 pb-5">
                  {/* 玩家负载进度条 */}
                  <div className="space-y-2">
                    <div className="flex justify-between text-xs">
                      <span className="text-muted-foreground flex items-center gap-1.5">
                        <Cpu className="h-3.5 w-3.5 text-primary" /> {t("active_load")}
                      </span>
                      <span className="font-medium text-foreground">
                        {counts.players} / {statusStats.maxPlayers} {t("stat_players_online")} ({loadPercentage}%)
                      </span>
                    </div>
                    <div className="h-2 w-full bg-secondary rounded-full overflow-hidden">
                      <div
                        className="bg-primary h-full rounded-full transition-all"
                        style={{ width: `${loadPercentage}%` }}
                      />
                    </div>
                  </div>

                  {/* 服务器状态分布健康条 */}
                  <div className="space-y-2">
                    <div className="flex justify-between text-xs">
                      <span className="text-muted-foreground">{t("status_distribution")}</span>
                      <span className="font-medium text-foreground">{t("instances_count", { count: servers.length })}</span>
                    </div>
                    {servers.length > 0 ? (
                      <div className="h-3 w-full bg-secondary rounded-full overflow-hidden flex">
                        {renderBarSegment(statusStats.running, "bg-[#22c55e]")}
                        {renderBarSegment(statusStats.starting, "bg-[#eab308]")}
                        {renderBarSegment(statusStats.stopping, "bg-[#f97316]")}
                        {renderBarSegment(statusStats.failed, "bg-[#ef4444]")}
                        {renderBarSegment(statusStats.stopped, "bg-[#64748b]")}
                        {renderBarSegment(statusStats.unknown, "bg-[#94a3b8]")}
                      </div>
                    ) : (
                      <div className="text-xs text-muted-foreground/60 py-1 text-center bg-muted/20 rounded">
                        {t("no_servers_monitor")}
                      </div>
                    )}
                  </div>

                  {/* 镜像及规格说明 */}
                  <div className="flex items-center gap-3 p-3 rounded-lg border bg-accent/5 text-xs text-muted-foreground">
                    <ShieldCheck className="h-4.5 w-4.5 text-primary shrink-0" />
                    <div>
                      <span className="font-semibold text-foreground mr-1">{totalImagesCount}</span>
                      {t("images_count")}{t("images_security_note")}
                    </div>
                  </div>
                </CardContent>
              </Card>

              {/* 平台环境与配置参数卡片 (flex-1 自动拉伸) */}
              <Card className="flex-1 flex flex-col justify-between">
                <CardHeader className="pb-3 pt-5 px-6">
                  <CardTitle className="text-base font-semibold text-foreground">
                    {t("system_specs")}
                  </CardTitle>
                </CardHeader>
                <CardContent className="px-6 pb-6 pt-1 flex-1 flex flex-col justify-between">
                  <div className="grid grid-cols-2 sm:grid-cols-3 gap-x-8 gap-y-6">
                    {/* 镜像总数 */}
                    <div className="space-y-1">
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <ShieldCheck className="h-3.5 w-3.5 text-primary" />
                        <span className="text-[11px] font-medium">{t("spec_images_total")}</span>
                      </div>
                      <div className="text-xl font-bold font-mono text-foreground leading-none mt-1.5">{totalImagesCount}</div>
                    </div>

                    {/* 集群内自建镜像 */}
                    <div className="space-y-1">
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <Box className="h-3.5 w-3.5 text-primary" />
                        <span className="text-[11px] font-medium">{t("spec_images_built")}</span>
                      </div>
                      <div className="text-xl font-bold font-mono text-foreground leading-none mt-1.5">{builtImagesCount}</div>
                    </div>

                    {/* 外部登记导入镜像 */}
                    <div className="space-y-1">
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <Database className="h-3.5 w-3.5 text-primary" />
                        <span className="text-[11px] font-medium">{t("spec_images_external")}</span>
                      </div>
                      <div className="text-xl font-bold font-mono text-foreground leading-none mt-1.5">{externalImagesCount}</div>
                    </div>

                    {/* 可见服务器总数 */}
                    <div className="space-y-1">
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <Server className="h-3.5 w-3.5 text-primary" />
                        <span className="text-[11px] font-medium">{t("spec_servers_total")}</span>
                      </div>
                      <div className="text-xl font-bold font-mono text-foreground leading-none mt-1.5">{counts.total}</div>
                    </div>

                    {/* 我拥有管理的服务器 */}
                    <div className="space-y-1">
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <CheckCircle className="h-3.5 w-3.5 text-primary" />
                        <span className="text-[11px] font-medium">{t("spec_servers_owned")}</span>
                      </div>
                      <div className="text-xl font-bold font-mono text-foreground leading-none mt-1.5">{ownedServersCount}</div>
                    </div>

                    {/* 公网可认领服务器 */}
                    <div className="space-y-1">
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <Hand className="h-3.5 w-3.5 text-primary" />
                        <span className="text-[11px] font-medium">{t("spec_servers_claimable")}</span>
                      </div>
                      <div className="text-xl font-bold font-mono text-foreground leading-none mt-1.5">{claimableServersCount}</div>
                    </div>

                    {/* 运行中实例占比 */}
                    <div className="space-y-1">
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <Activity className="h-3.5 w-3.5 text-primary" />
                        <span className="text-[11px] font-medium">{t("spec_running_ratio")}</span>
                      </div>
                      <div className="text-xl font-bold font-mono text-[#22c55e] leading-none mt-1.5">{runningRatio}%</div>
                    </div>

                    {/* 节点休眠率 */}
                    <div className="space-y-1">
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <Activity className="h-3.5 w-3.5 text-primary" />
                        <span className="text-[11px] font-medium">{t("spec_stopped_ratio")}</span>
                      </div>
                      <div className="text-xl font-bold font-mono text-slate-500 leading-none mt-1.5">{stoppedRatio}%</div>
                    </div>

                    {/* 运行故障率 */}
                    <div className="space-y-1">
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <AlertTriangle className="h-3.5 w-3.5 text-primary" />
                        <span className="text-[11px] font-medium">{t("spec_failed_ratio")}</span>
                      </div>
                      <div className="text-xl font-bold font-mono text-rose-500 leading-none mt-1.5">{failedRatio}%</div>
                    </div>
                  </div>
                </CardContent>
              </Card>
            </>
          ) : (
            <Card className="overflow-hidden flex flex-col justify-between flex-1">
              <CardHeader className="pb-3 pt-5 px-6">
                <CardTitle className="text-base font-semibold text-foreground">
                  {t("welcome_title")}
                </CardTitle>
              </CardHeader>
              <CardContent className="px-6 pb-6 pt-1 flex-1 flex flex-col justify-between gap-4">
                <p className="text-xs text-muted-foreground leading-relaxed">
                  {t("welcome_desc")}
                </p>

                {/* 极简平台新手引导指引（真实且有用） */}
                <div className="p-3.5 rounded-lg border bg-accent/5 text-[11px] text-muted-foreground space-y-1.5">
                  <div className="font-semibold text-foreground text-xs">{t("quick_start_guide")}</div>
                  <ul className="list-disc list-inside space-y-1">
                    <li>{t("quick_start_step1")}</li>
                    <li>{t("quick_start_step2")}</li>
                    <li>{t("quick_start_step3")}</li>
                  </ul>
                </div>

                <div>
                  <Link to="/servers">
                    <Button variant="default" size="sm" className="text-xs gap-1.5 w-full sm:w-auto">
                      {t("go_to_servers_btn")} <ArrowRight className="w-3.5 h-3.5" />
                    </Button>
                  </Link>
                </div>
              </CardContent>
            </Card>
          )}
        </div>

        {/* 右侧：3D星图拓扑监控 (lg:col-span-4 h-full) */}
        <Card className="lg:col-span-4 flex flex-col overflow-hidden h-full">
          <CardHeader className="space-y-1 pb-4">
            <CardTitle className="text-base">{t("fleet")}</CardTitle>
          </CardHeader>
          <CardContent className="flex-1 flex flex-col p-0 justify-between">
            <div className="flex-1 min-h-[220px] w-full bg-gradient-to-b from-transparent to-primary/5 border-b relative">
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
            {/* 状态图例说明 + 指标统计 */}
            <div className="p-4 space-y-3 text-xs bg-muted/10">
              <div className="font-semibold text-muted-foreground text-[10px] uppercase tracking-wider">
                {t("legend_title")}
              </div>
              <div className="grid grid-cols-2 gap-x-4 gap-y-2">
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <span className="h-2 w-2 rounded-full bg-[#22c55e]" />
                    <span className="text-muted-foreground text-[11px]">{t("legend_running")}</span>
                  </div>
                  <span className="font-mono text-[11px] text-foreground font-semibold">{statusStats.running}</span>
                </div>
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <span className="h-2 w-2 rounded-full bg-[#eab308] animate-pulse" />
                    <span className="text-muted-foreground text-[11px]">{t("legend_starting")}</span>
                  </div>
                  <span className="font-mono text-[11px] text-foreground font-semibold">{statusStats.starting}</span>
                </div>
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <span className="h-2 w-2 rounded-full bg-[#f97316] animate-pulse" />
                    <span className="text-muted-foreground text-[11px]">{t("legend_stopping")}</span>
                  </div>
                  <span className="font-mono text-[11px] text-foreground font-semibold">{statusStats.stopping}</span>
                </div>
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <span className="h-2 w-2 rounded-full bg-[#64748b]" />
                    <span className="text-muted-foreground text-[11px]">{t("legend_stopped")}</span>
                  </div>
                  <span className="font-mono text-[11px] text-foreground font-semibold">{statusStats.stopped}</span>
                </div>
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <span className="h-2 w-2 rounded-full bg-[#ef4444]" />
                    <span className="text-muted-foreground text-[11px]">{t("legend_failed")}</span>
                  </div>
                  <span className="font-mono text-[11px] text-foreground font-semibold">{statusStats.failed}</span>
                </div>
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    <span className="h-2 w-2 rounded-full bg-[#94a3b8]" />
                    <span className="text-muted-foreground text-[11px]">{t("legend_unknown")}</span>
                  </div>
                  <span className="font-mono text-[11px] text-foreground font-semibold">{statusStats.unknown}</span>
                </div>
              </div>
            </div>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
