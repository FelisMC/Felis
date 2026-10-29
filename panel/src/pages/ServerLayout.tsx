import { Outlet, useMatch, useOutletContext, useParams } from "react-router-dom";
import { Activity, Archive, CalendarClock, FolderOpen, Puzzle, Settings, Shield, Terminal, Users } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { BackLink } from "@/components/BackLink";
import { CopyAddress } from "@/components/CopyAddress";
import { softwareSummary } from "@/components/ServerConfigForm";
import { MessageLine } from "@/components/MessageLine";
import { PageHeader } from "@/components/PageHeader";
import { LinkTabs, type LinkTab } from "@/components/PageTabs";
import { PhaseBadge, pendingPower, shownPhase, startFailure } from "@/components/PhaseBadge";
import { PowerButton } from "@/components/PowerButton";
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

/** ServerLayout frames a server's pages with its header, power controls and section tabs. */
export function ServerLayout() {
  const { name = "" } = useParams();
  const cfg = useConfig();
  const { isAdmin, isOwner } = useTier();
  const { t } = useTranslation("servers");
  const { data, error, loading, reload, updatedAt } = useAsync(() => api.status(name), [name], { coalesce: true });
  // Poll faster during power transitions and retry backoff.
  const moving = !!data && (data.phase === "Starting" || data.phase === "Stopping" || pendingPower(data) !== null || startFailure(data) === "retrying");
  usePolling(reload, moving ? STATUS_POLL_FAST_MS : STATUS_POLL_SLOW_MS);
  const { data: mine } = useAsync(() => (isAdmin ? Promise.resolve([]) : api.myServers()), [isAdmin, name]);
  const owned = canManage(isAdmin, mine, name);
  const onConsole = useMatch({ path: "/servers/:name", end: true }) !== null;

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

  // Only the console fills the window; the other tabs grow with their content so the page keeps its bottom spacing.
  return (
    <div className={cn("flex flex-col gap-4", onConsole && "min-h-0 flex-1")}>
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
              <span role="status" className="text-sm font-medium text-destructive">{t("status_stale")}</span>
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
        tabs={<LinkTabs tabs={tabs} label={t("server_tabs")} />}
        back={backLink}
      />
      <Outlet context={context} />
    </div>
  );
}

export function HostRecovery({ name }: { name: string }) {
  const { t } = useTranslation("servers");
  if (!/^[a-z0-9][a-z0-9-]{0,31}$/.test(name)) return null;
  return <details className="rounded-xl border border-border bg-card p-3 text-sm"><summary className="cursor-pointer font-medium">{t("host_recovery")}</summary><p className="my-3 text-muted-foreground">{t("host_recovery_hint")}</p><pre className="overflow-x-auto whitespace-pre-wrap rounded bg-muted p-3 text-xs">{`kubectl patch minecraftserver ${name} -n minecraft --type=merge -p '{"spec":{"desiredState":"Stopped"}}'
kubectl scale statefulset ${name} -n minecraft --replicas=0
kubectl get pods -n minecraft -l felis.lolicon.best/server=${name},felis.lolicon.best/component=server -o wide`}</pre><p className="mt-3 text-muted-foreground">{t("host_recovery_verify")}</p></details>;
}
