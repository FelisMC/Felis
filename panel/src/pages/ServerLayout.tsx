import { useContext, useEffect, useRef } from "react";
import { createPortal } from "react-dom";
import { Link, NavLink, Outlet, useLocation, useMatch, useOutletContext, useParams } from "react-router-dom";
import { Activity, Archive, ArrowLeft, CalendarClock, FolderOpen, Lock, Puzzle, Settings, Shield, ShieldCheck, Terminal, Users } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { BackLink } from "@/components/BackLink";
import { CopyAddress } from "@/components/CopyAddress";
import { softwareSummary } from "@/components/ServerConfigForm";
import { MessageLine } from "@/components/MessageLine";
import { PageHeader } from "@/components/PageHeader";
import { LinkTabs, type LinkTab } from "@/components/PageTabs";
import { PhaseBadge, pendingPower, phaseColor, shownPhase, startFailure } from "@/components/PhaseBadge";
import { PowerButton } from "@/components/PowerButton";
import { ShellContext } from "@/components/ShellContext";
import { ErrorState, Loading } from "@/components/States";
import { api, humanizeError } from "@/lib/api";
import { joinAddress } from "@/lib/config";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS, useAsync, useConfig, usePolling } from "@/lib/hooks";
import { canManageRole, canOperateRole, isReadonlyRole, resolveServerAccessRole, type EffectiveAccessRole } from "@/lib/ownership";
import { useTier } from "@/lib/tier";
import type { ServerStatus } from "@/lib/types";
import { cn } from "@/lib/utils";
import { useEnterAnimation, useSlidingIndicator } from "@/lib/motion";

/** ServerContext is what every tab of one server shares: its polled status and who may manage it. */
export interface ServerContext {
  name: string;
  data: ServerStatus;
  /** error is the last refresh failure while data still holds the previous answer. */
  error: unknown;
  reload: () => void;
  owned: boolean;
  actualOwned: boolean;
  accessRole: EffectiveAccessRole;
  canOperate: boolean;
  canManage: boolean;
}

export function useServer(): ServerContext {
  return useOutletContext<ServerContext>();
}

const isSystem = (name: string) => name === "login" || name === "lobby";

function ServerSidebarRail({
  data,
  tabs,
  backLabel,
  navLabel,
}: {
  data: ServerStatus;
  tabs: LinkTab[];
  backLabel: string;
  navLabel: string;
}) {
  const { t } = useTranslation("servers");
  const { pathname } = useLocation();
  const navRef = useRef<HTMLElement>(null);
  const indicator = useSlidingIndicator(navRef, '[aria-current="page"]', `${pathname}:${tabs.length}`, "y");
  const shown = shownPhase(data);
  return (
    <div className="flex animate-fade-in flex-col">
      <Link
        to="/servers"
        className="group -mt-1 mb-1.5 flex items-center gap-1.5 border-b border-border/60 px-4 py-2 text-xs text-muted-foreground transition-colors duration-150 hover:bg-accent/55 hover:text-foreground"
      >
        <ArrowLeft className="h-3.5 w-3.5 shrink-0 transition-transform duration-150 ease-out group-hover:-translate-x-0.5" />
        <span className="truncate">{backLabel}</span>
      </Link>
      <div className="px-4 py-2">
        <div className="flex items-center gap-1.5 text-[12.5px] font-semibold text-foreground">
          <span
            aria-hidden="true"
            className="h-[5.5px] w-[5.5px] shrink-0 rounded-full transition-colors duration-200"
            style={{ backgroundColor: phaseColor(shown) }}
          />
          <span className="truncate">{data.displayName || data.name}</span>
        </div>
        <div className="mt-0.5 truncate font-mono text-[10.5px] text-muted-foreground">
          {data.name}
          {data.phase === "Running" && !data.playerCountUnknown
            ? ` · ${t("header_players", { online: data.playersOnline, max: data.playersMax })}`
            : ""}
        </div>
      </div>
      <nav ref={navRef} aria-label={navLabel} className="relative mt-1 flex flex-col">
        {indicator.ready && (
          <span
            aria-hidden="true"
            className={cn(
              "pointer-events-none absolute inset-x-0 top-0 z-0 bg-accent before:absolute before:inset-y-1 before:left-0 before:w-[2.5px] before:rounded-r-sm before:bg-primary motion-reduce:transition-none",
              indicator.animate && "transition-glide-y",
            )}
            style={{
              transform: `translateY(${indicator.start}px)`,
              height: `${indicator.size}px`,
            }}
          />
        )}
        {tabs.map(({ to, label, icon: Icon, end }) => (
          <NavLink
            key={to}
            to={to}
            end={end}
            className={({ isActive }) =>
              cn(
                "group relative z-10 flex items-center gap-2.5 px-4 py-[6.5px] text-[12.5px] transition-[color,background-color,transform] duration-150 ease-out active:scale-[0.98]",
                isActive
                  ? cn(
                      "font-medium text-foreground",
                      !indicator.ready && "bg-accent before:absolute before:bottom-1 before:left-0 before:top-1 before:w-[2.5px] before:rounded-r-sm before:bg-primary",
                    )
                  : "text-muted-foreground hover:bg-accent/55 hover:text-foreground",
              )
            }
          >
            {({ isActive }) => (
              <>
                {Icon && (
                  <Icon
                    className={cn(
                      "h-3.5 w-3.5 shrink-0 transition-[color,opacity] duration-150",
                      isActive ? "text-foreground opacity-95" : "opacity-65 group-hover:opacity-90",
                    )}
                    strokeWidth={1.7}
                  />
                )}
                <span className="truncate">{label}</span>
              </>
            )}
          </NavLink>
        ))}
      </nav>
    </div>
  );
}

/** ServerLayout frames a server's pages with its header, power controls and section tabs. */
export function ServerLayout() {
  const { name = "" } = useParams();
  const { pathname } = useLocation();
  const outletRef = useRef<HTMLDivElement>(null);
  const cfg = useConfig();
  const { isAdmin, isOwner } = useTier();
  const { t } = useTranslation("servers");
  const shell = useContext(ShellContext);
  const setActiveServer = shell?.setActiveServer;
  const { data, error, loading, reload, updatedAt } = useAsync(() => api.status(name), [name], { coalesce: true });
  useEnterAnimation(outletRef, pathname, -6, 200, 0.4);
  // Poll faster during power transitions and retry backoff.
  const moving = !!data && (data.phase === "Starting" || data.phase === "Stopping" || data.phase === "Hibernating" ||
    (!!data.hibernation && data.hibernation.stage !== "Frozen") || pendingPower(data) !== null || startFailure(data) === "retrying");
  usePolling(reload, moving ? STATUS_POLL_FAST_MS : STATUS_POLL_SLOW_MS);
  const { data: mine } = useAsync(() => (isAdmin ? Promise.resolve([]) : api.myServers()), [isAdmin, name]);
  const actualOwned = (mine ?? []).some((s) => s.name === name && s.owned === true);
  const accessRole = data ? resolveServerAccessRole(isAdmin, data, mine, name) : "none";
  const canOperate = canOperateRole(accessRole);
  const canManageAccess = canManageRole(accessRole);
  const readonly = isReadonlyRole(accessRole);
  const onConsole = useMatch({ path: "/servers/:name", end: true }) !== null;

  const currentPhase = data ? shownPhase(data) : undefined;
  const displayName = data?.displayName;
  useEffect(() => {
    if (!setActiveServer) return;
    setActiveServer({ name, displayName, phase: currentPhase });
    return () => setActiveServer(null);
  }, [setActiveServer, name, displayName, currentPhase]);

  const backLink = <BackLink to="/servers" label={t("my_servers_breadcrumb")} />;
  const back = <div>{backLink}</div>;
  if (loading && !data) return <div className="flex flex-col gap-4">{back}<Loading /></div>;
  if (!data) {
    return (
      <div className="flex flex-col gap-4">
        {back}
        <div className="space-y-3"><ErrorState error={error} onRetry={reload} />{isOwner && <HostRecovery name={name} />}</div>
      </div>
    );
  }

  const failure = startFailure(data);
  const startup = data.startup;
  const startupBlocked = startup?.stage === "failed" || startup?.reason === "Unschedulable";
  const base = `/servers/${name}`;
  const allTabs: LinkTab[] = [
    { to: base, end: true, label: t("tab_console"), icon: Terminal },
    { to: `${base}/monitor`, label: t("tab_monitor"), icon: Activity },
    { to: `${base}/files`, label: t("tab_files"), icon: FolderOpen },
    { to: `${base}/players`, label: t("tab_players"), icon: Users },
    { to: `${base}/backups`, label: t("tab_backups"), icon: Archive },
    { to: `${base}/schedules`, label: t("tab_schedules"), icon: CalendarClock },
    ...(canManageAccess ? [
      { to: `${base}/extensions`, label: t("tab_extensions"), icon: Puzzle },
      { to: `${base}/luckperms`, label: t("tab_luckperms"), icon: Shield },
    ] : []),
    ...(canManageAccess || (isOwner && !isSystem(name)) ? [{ to: `${base}/settings`, label: t("tab_settings"), icon: Settings }] : []),
  ];
  const operatorTabs: LinkTab[] = [
    { to: base, end: true, label: t("tab_console"), icon: Terminal },
    { to: `${base}/monitor`, label: t("tab_monitor"), icon: Activity },
    { to: `${base}/players`, label: t("tab_players"), icon: Users },
    { to: `${base}/backups`, label: t("tab_backups"), icon: Archive },
  ];
  const tabs: LinkTab[] = readonly
    ? [{ to: base, end: true, label: t("tab_overview"), icon: Terminal }]
    : accessRole === "operator"
      ? operatorTabs
      : allTabs;
  const facts = [
    data.software && softwareSummary(t, data.software),
    data.phase === "Running" && !data.playerCountUnknown && t("header_players", { online: data.playersOnline, max: data.playersMax }),
  ].filter(Boolean);
  const context: ServerContext = {
    name,
    data,
    error,
    reload,
    owned: canManageAccess,
    actualOwned,
    accessRole,
    canOperate,
    canManage: canManageAccess,
  };
  const inShellRail = Boolean(shell?.sidebarSlot);

  // Only the console fills the window; the other tabs grow with their content so the page keeps its bottom spacing.
  return (
    <div className={cn("flex flex-col gap-4", onConsole && "min-h-0 flex-1")}>
      {shell?.sidebarSlot &&
        createPortal(
          <ServerSidebarRail
            data={data}
            tabs={tabs}
            backLabel={t("my_servers_breadcrumb")}
            navLabel={t("server_tabs")}
          />,
          shell.sidebarSlot,
        )}
      {error != null && (
        <div className="space-y-2">
          <MessageLine kind="error" message={t("status_stale_detail", { time: updatedAt ? new Date(updatedAt).toLocaleTimeString() : "—", error: humanizeError(error) })} />
          {isOwner && <HostRecovery name={name} />}
        </div>
      )}
      {readonly && (
        <div
          role="status"
          className="flex items-start gap-2.5 rounded-lg border border-amber-500/35 bg-amber-500/10 px-3.5 py-2.5 text-sm"
        >
          <Lock className="mt-0.5 h-4 w-4 shrink-0 text-amber-600 dark:text-amber-400" aria-hidden="true" />
          <div className="space-y-0.5">
            <p className="font-medium text-foreground">{t("protected_readonly_title")}</p>
            <p className="text-xs text-muted-foreground">{t("protected_readonly_body")}</p>
          </div>
        </div>
      )}
      <PageHeader
        icon={Terminal}
        title={data.displayName || data.name}
        subtitle={
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            {cfg && <CopyAddress address={joinAddress(data.subdomain, cfg)} />}
            {facts.map((fact) => <span key={String(fact)} className="text-xs">{fact}</span>)}
          </div>
        }
        actions={
          <div className="flex min-w-0 max-w-full flex-wrap items-center justify-end gap-2">
            {data.protected && (
              <Badge variant="warning" className="gap-1">
                <ShieldCheck className="h-3 w-3" aria-hidden="true" />
                {t("protected_badge")}
              </Badge>
            )}
            {error != null ? (
              <span role="status" className="text-xs font-medium text-destructive">{t("status_stale")}</span>
            ) : startupBlocked ? (
              <Badge variant="outline" className="border-amber-500/30 text-amber-600 dark:text-amber-400">
                {t(startup?.stage === "failed" ? "startup_problem" : "startup_waiting_resources")}
              </Badge>
            ) : (
              <PhaseBadge phase={shownPhase(data)} failure={failure} autoRestarts={data.autoRestarts} />
            )}
            {error == null && canOperate && (
              <PowerButton
                name={name}
                phase={data.phase}
                desiredState={data.desiredState}
                failed={failure !== null}
                playersOnline={data.playersOnline}
                playerCountUnknown={data.playerCountUnknown}
                retiring={data.retiring}
                diskFull={data.diskFull}
                allowRestart={canOperate}
                onChanged={reload}
              />
            )}
          </div>
        }
        tabs={inShellRail ? <div className="md:hidden"><LinkTabs tabs={tabs} label={t("server_tabs")} /></div> : <LinkTabs tabs={tabs} label={t("server_tabs")} />}
        back={inShellRail ? undefined : backLink}
      />
      <div ref={outletRef} className={cn("flex flex-col gap-4", onConsole && "min-h-0 flex-1")}>
        <Outlet context={context} />
      </div>
    </div>
  );
}

export function HostRecovery({ name }: { name: string }) {
  const { t } = useTranslation("servers");
  if (!/^[a-z0-9][a-z0-9-]{0,31}$/.test(name)) return null;
  return <details className="rounded-lg border border-border bg-card p-3 text-sm"><summary className="cursor-pointer font-medium">{t("host_recovery")}</summary><p className="my-3 text-muted-foreground">{t("host_recovery_hint")}</p><pre className="overflow-x-auto whitespace-pre-wrap rounded bg-muted p-3 text-xs">{`kubectl patch minecraftserver ${name} -n minecraft --type=merge -p '{"spec":{"desiredState":"Stopped"}}'
kubectl scale statefulset ${name} -n minecraft --replicas=0
kubectl get pods -n minecraft -l felis.lolicon.best/server=${name},felis.lolicon.best/component=server -o wide`}</pre><p className="mt-3 text-muted-foreground">{t("host_recovery_verify")}</p></details>;
}
