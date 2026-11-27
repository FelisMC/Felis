import { useContext, useEffect } from "react";
import { createPortal } from "react-dom";
import { Link, NavLink, Outlet, useMatch, useOutletContext, useParams } from "react-router-dom";
import { Activity, Archive, ArrowLeft, CalendarClock, FolderOpen, Puzzle, Settings, Shield, Terminal, Users } from "lucide-react";
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
import { canManage } from "@/lib/ownership";
import { useTier } from "@/lib/tier";
import type { ServerStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

/** ServerContext is what every tab of one server shares: its polled status and who may manage it. */
export interface ServerContext {
  name: string;
  data: ServerStatus;
  /** error is the last refresh failure while data still holds the previous answer. */
  error: unknown;
  reload: () => void;
  owned: boolean;
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
  const shown = shownPhase(data);
  return (
    <div className="flex flex-col">
      <Link
        to="/servers"
        className="-mt-1 mb-1.5 flex items-center gap-1.5 border-b border-border/60 px-4 py-2 text-xs text-muted-foreground transition-colors hover:bg-accent/55 hover:text-foreground"
      >
        <ArrowLeft className="h-3.5 w-3.5 shrink-0" />
        <span className="truncate">{backLabel}</span>
      </Link>
      <div className="px-4 py-2">
        <div className="flex items-center gap-1.5 text-[12.5px] font-semibold text-foreground">
          <span
            aria-hidden="true"
            className="h-[5.5px] w-[5.5px] shrink-0 rounded-full"
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
      <nav aria-label={navLabel} className="mt-1 flex flex-col">
        {tabs.map(({ to, label, icon: Icon, end }) => (
          <NavLink
            key={to}
            to={to}
            end={end}
            className={({ isActive }) =>
              cn(
                "group relative flex items-center gap-2.5 px-4 py-[6.5px] text-[12.5px] transition-colors",
                isActive
                  ? "bg-accent font-medium text-foreground before:absolute before:bottom-1 before:left-0 before:top-1 before:w-[2.5px] before:rounded-r-sm before:bg-primary"
                  : "text-muted-foreground hover:bg-accent/55 hover:text-foreground",
              )
            }
          >
            {({ isActive }) => (
              <>
                {Icon && (
                  <Icon
                    className={cn("h-3.5 w-3.5 shrink-0", isActive ? "text-foreground opacity-95" : "opacity-65 group-hover:opacity-90")}
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
  const cfg = useConfig();
  const { isAdmin, isOwner } = useTier();
  const { t } = useTranslation("servers");
  const shell = useContext(ShellContext);
  const setActiveServer = shell?.setActiveServer;
  const { data, error, loading, reload, updatedAt } = useAsync(() => api.status(name), [name], { coalesce: true });
  // Poll faster during power transitions and retry backoff.
  const moving = !!data && (data.phase === "Starting" || data.phase === "Stopping" || data.phase === "Hibernating" ||
    (!!data.hibernation && data.hibernation.stage !== "Frozen") || pendingPower(data) !== null || startFailure(data) === "retrying");
  usePolling(reload, moving ? STATUS_POLL_FAST_MS : STATUS_POLL_SLOW_MS);
  const { data: mine } = useAsync(() => (isAdmin ? Promise.resolve([]) : api.myServers()), [isAdmin, name]);
  const owned = canManage(isAdmin, mine, name);
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
  const tabs: LinkTab[] = [
    { to: base, end: true, label: t("tab_console"), icon: Terminal },
    { to: `${base}/monitor`, label: t("tab_monitor"), icon: Activity },
    { to: `${base}/files`, label: t("tab_files"), icon: FolderOpen },
    { to: `${base}/players`, label: t("tab_players"), icon: Users },
    { to: `${base}/backups`, label: t("tab_backups"), icon: Archive },
    { to: `${base}/schedules`, label: t("tab_schedules"), icon: CalendarClock },
    ...(owned ? [
      { to: `${base}/extensions`, label: t("tab_extensions"), icon: Puzzle },
      { to: `${base}/luckperms`, label: t("tab_luckperms"), icon: Shield },
    ] : []),
    ...(owned || isAdmin || (isOwner && !isSystem(name)) ? [{ to: `${base}/settings`, label: t("tab_settings"), icon: Settings }] : []),
  ];
  const facts = [
    data.software && softwareSummary(t, data.software),
    data.phase === "Running" && !data.playerCountUnknown && t("header_players", { online: data.playersOnline, max: data.playersMax }),
  ].filter(Boolean);
  const context: ServerContext = { name, data, error, reload, owned };
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
            {error != null ? (
              <span role="status" className="text-xs font-medium text-destructive">{t("status_stale")}</span>
            ) : startupBlocked ? (
              <Badge variant="outline" className="border-amber-500/30 text-amber-600 dark:text-amber-400">
                {t(startup?.stage === "failed" ? "startup_problem" : "startup_waiting_resources")}
              </Badge>
            ) : (
              <PhaseBadge phase={shownPhase(data)} failure={failure} autoRestarts={data.autoRestarts} />
            )}
            {error == null && (
              <PowerButton
                name={name}
                phase={data.phase}
                desiredState={data.desiredState}
                failed={failure !== null}
                playersOnline={data.playersOnline}
                playerCountUnknown={data.playerCountUnknown}
                retiring={data.retiring}
                diskFull={data.diskFull}
                allowRestart={owned}
                onChanged={reload}
              />
            )}
          </div>
        }
        tabs={inShellRail ? <div className="md:hidden"><LinkTabs tabs={tabs} label={t("server_tabs")} /></div> : <LinkTabs tabs={tabs} label={t("server_tabs")} />}
        back={inShellRail ? undefined : backLink}
      />
      <Outlet context={context} />
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
