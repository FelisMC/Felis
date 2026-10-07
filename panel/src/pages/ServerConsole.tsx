import { useState, useRef, useCallback, useId, useLayoutEffect, type KeyboardEvent } from "react";
import { Link, useParams } from "react-router-dom";
import { Terminal, Moon, Shield, ShieldAlert, HelpCircle, Loader2, Users, Archive, FolderOpen, CalendarClock, ChevronRight, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { BackLink } from "@/components/BackLink";
import { MAX_AUTO_RESTARTS, PhaseBadge, pendingPower, shownPhase, startFailure, type StartFailure } from "@/components/PhaseBadge";
import { PageHeader } from "@/components/PageHeader";
import { FormattedText, LogConsole } from "@/components/LogConsole";
import { Loading, ErrorState } from "@/components/States";
import { api, consoleStreamURL, humanizeError } from "@/lib/api";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS, useAsync, useConfig, usePolling } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage } from "@/lib/ownership";
import { joinAddress } from "@/lib/config";
import { CopyAddress } from "@/components/CopyAddress";
import type { Phase, AutostartPolicy } from "@/lib/types";
import { EditServerDialog } from "@/components/EditServerDialog";
import { PowerButton } from "@/components/PowerButton";
import { RetireCard, RetireNotice } from "@/components/Retirement";
import { parseFormatting, type Formatted } from "@/lib/mcformat";
import { cn } from "@/lib/utils";
import { InlineError, MessageLine } from "@/components/MessageLine";

// notStreamingCopy explains why there is no live feed for a phase that has no
// streamable pod. The read path only has something to relay once a pod is up, so
// Stopped/Failed/Stopping each get their own honest line rather than an empty
// console. `default` covers Unknown plus any future phase the backend may emit
// that the panel hasn't modelled yet — the screen stays informative regardless.
function useNotStreamingCopy(phase: Phase, waitingForContainer: boolean): { icon: LucideIcon; title: string; body: string; spin?: boolean } {
  const { t } = useTranslation("servers");
  if (waitingForContainer) return { icon: HelpCircle, title: t("logs_unavailable_title"), body: t("logs_unavailable_body") };
  switch (phase) {
    case "Starting":
      // Asked to start with no pod yet: the stream attaches once the pod is up.
      return {
        icon: Loader2,
        title: t("server_waking_title"),
        body: t("server_waking_body"),
        spin: true,
      };
    case "Stopped":
      return {
        icon: Moon,
        title: t("server_asleep_title"),
        body: t("server_asleep_body"),
      };
    case "Stopping":
      return {
        icon: Moon,
        title: t("server_shutting_down_title"),
        body: t("server_shutting_down_body"),
      };
    default:
      return {
        icon: HelpCircle,
        title: t("console_offline_title"),
        body: t("console_offline_body"),
      };
  }
}

function NotStreaming({ phase, waitingForContainer = false }: { phase: Phase; waitingForContainer?: boolean }) {
  const { icon: Icon, title, body, spin } = useNotStreamingCopy(phase, waitingForContainer);
  return (
    <div className="flex items-start gap-3 rounded-md border border-dashed border-border bg-muted/30 p-6 text-sm text-muted-foreground">
      <Icon className={cn("mt-0.5 h-5 w-5 shrink-0 text-muted-foreground/70", spin && "animate-spin")} />
      <div className="space-y-1">
        <p className="font-medium text-foreground">{title}</p>
        <p>{body}</p>
      </div>
    </div>
  );
}

// FailedStartNotice heads the console of a server whose start failed: whether the
// operator will try again on its own, and what the owner can do. The log below it
// is the attempt that failed, which is what they need to find the cause.
function FailedStartNotice({ failure, autoRestarts }: { failure: StartFailure; autoRestarts: number }) {
  const { t } = useTranslation("servers");
  const retrying = failure === "retrying";
  const titleId = useId();
  return (
    <div
      role="status"
      aria-labelledby={titleId}
      className="flex items-start gap-3 border-b border-zinc-800 bg-red-950/40 px-4 py-3 text-sm text-zinc-300"
    >
      <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0 text-red-400" />
      <div className="space-y-0.5">
        <p id={titleId} className="font-medium text-zinc-100">
          {retrying ? t("server_failed_retrying_title") : t("server_failed_title")}
        </p>
        <p>
          {retrying
            ? t("server_failed_retrying_body", { used: autoRestarts, max: MAX_AUTO_RESTARTS })
            : t("server_failed_body")}
        </p>
      </div>
    </div>
  );
}

function loadHistory(name: string): string[] {
  try {
    const raw = localStorage.getItem(HISTORY_KEY(name));
    return raw ? JSON.parse(raw) : [];
  } catch {
    return [];
  }
}

function persistHistory(name: string, h: string[]): void {
  try {
    localStorage.setItem(HISTORY_KEY(name), JSON.stringify(h));
  } catch {
    /* storage full — silently drop */
  }
}

const MAX_HISTORY = 50;
const HISTORY_KEY = (name: string) => `felis:cmd:history:${name}`;

/** CommandInput is the §8 write-side console input: a one-line text field that
 *  sends an RCON command to the running server and displays its formatted reply.
 *  Enter sends; Up/Down cycle through persistent per-server command history.
 *  Only available when the server is Running (RCON reachable). */
function CommandInput({ name }: { name: string }) {
  const { t } = useTranslation("servers");
  const [command, setCommand] = useState("");
  const [sending, setSending] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // The last command's echo + the server's reply, rendered above the prompt the
  // way a terminal does. The reply IS the result of a write: RCON returns text
  // only, so dropping it would leave the user with no way to see what happened.
  const [last, setLast] = useState<{ cmd: string; out: Formatted } | null>(null);
  const historyRef = useRef<string[]>(loadHistory(name));
  const cursorRef = useRef(-1);
  // What was typed before stepping up into the history.
  const draftRef = useRef("");
  const inputRef = useRef<HTMLInputElement>(null);
  const composingRef = useRef(false);

  const send = useCallback(async (text: string) => {
    if (!text || sending) return;
    setSending(true);
    setErr(null);
    setCommand("");
    const h = historyRef.current;
    if (h.length === 0 || h[h.length - 1] !== text) {
      if (h.length >= MAX_HISTORY) h.shift();
      h.push(text);
      persistHistory(name, h);
    }
    cursorRef.current = h.length;
    try {
      const res = await api.sendCommand(name, text);
      setLast({ cmd: text, out: parseFormatting((res.output ?? "").trim()) });
    } catch (ex) {
      setErr(humanizeError(ex));
    } finally {
      setSending(false);
    }
  }, [name, sending]);

  useLayoutEffect(() => {
    if (!sending) inputRef.current?.focus();
  }, [sending]);

  const onKeyDown = useCallback((e: KeyboardEvent<HTMLInputElement>) => {
    if (composingRef.current) return;
    const h = historyRef.current;
    if (e.key === "Enter") {
      e.preventDefault();
      send(command.trim());
      return;
    }
    if (h.length === 0) return;
    // The cursor is -1 or h.length on the line being typed, else on a history entry.
    const onLine = cursorRef.current === -1 || cursorRef.current >= h.length;
    if (e.key === "ArrowUp") {
      e.preventDefault();
      if (onLine) draftRef.current = command;
      const idx = Math.max(0, onLine ? h.length - 1 : cursorRef.current - 1);
      cursorRef.current = idx;
      setCommand(h[idx]);
      setTimeout(() => { inputRef.current?.setSelectionRange(h[idx].length, h[idx].length); }, 0);
    }
    // Down walks back toward the line being typed, and brings it back as it was;
    // on that line there is nothing below.
    if (e.key === "ArrowDown" && !onLine) {
      e.preventDefault();
      const idx = cursorRef.current + 1;
      cursorRef.current = idx;
      const next = idx < h.length ? h[idx] : draftRef.current;
      setCommand(next);
      setTimeout(() => { inputRef.current?.setSelectionRange(next.length, next.length); }, 0);
    }
  }, [command, send]);

  return (
    <div className="space-y-2">
      {last && (
        <pre className="max-h-40 overflow-y-auto whitespace-pre-wrap break-words rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2 font-mono text-xs text-zinc-300">
          <span className="text-zinc-500">{"> " + last.cmd + "\n"}</span>
          <FormattedText formatted={last.out} />
        </pre>
      )}
      <div className="flex items-center gap-2 rounded-md border border-zinc-700 bg-zinc-900 px-3 font-mono text-xs text-zinc-200">
        <span className="shrink-0 select-none text-zinc-500">{">"}</span>
        <input
          ref={inputRef}
          value={command}
          onChange={(e) => { setCommand(e.target.value); cursorRef.current = -1; }}
          onKeyDown={onKeyDown}
          onCompositionStart={() => { composingRef.current = true; }}
          onCompositionEnd={() => { composingRef.current = false; }}
          aria-label={t("command_label")}
          placeholder={t("command_placeholder")}
          autoComplete="off"
          spellCheck={false}
          className="flex-1 bg-transparent py-1.5 text-zinc-200 placeholder:text-zinc-500 focus:outline-none"
        />
        {sending && <Loader2 className="h-3.5 w-3.5 animate-spin shrink-0 text-zinc-500" />}
      </div>
      <InlineError message={err} className="text-xs" />
    </div>
  );
}

export function ServerConsole() {
  const { name = "" } = useParams();
  const cfg = useConfig();
  const { isAdmin, isOwner } = useTier();
  const { t } = useTranslation("servers");
  const { data, error, loading, reload, updatedAt } = useAsync(
    () => api.status(name),
    [name],
    { coalesce: true },
  );
  // The header, the power button and whether the stream attaches all follow the
  // status, so it is reread: fast while the server is on its way somewhere (a
  // wake or stop just sent, a pod starting, an automatic retry due), slow while
  // nothing is due, which still catches an idle stop or a wake from the lobby.
  const moving =
    !!data &&
    (data.phase === "Starting" ||
      data.phase === "Stopping" ||
      pendingPower(data) !== null ||
      startFailure(data) === "retrying");
  usePolling(reload, moving ? STATUS_POLL_FAST_MS : STATUS_POLL_SLOW_MS);
  const { data: mine } = useAsync(
    () => (isAdmin ? Promise.resolve([]) : api.myServers()),
    [isAdmin, name],
  );
  const owned = canManage(isAdmin, mine, name);

  // The read-side stream (spec §8) follows the running pod's log. A pod exists
  // during BOTH Starting and Running — the operator marks Starting once the pod
  // is up but RCON is not yet reachable (it only flips to Running after RCON
  // readiness). Boot logs flow precisely in that Starting window, which is when a
  // read most wants them, so the gate streams for both, not Running alone. A
  // Failed start usually leaves its pod behind, and that pod's log is how the
  // owner finds out why it failed, so Failed streams too.
  const streamable = data?.phase === "Running" || ((data?.phase === "Starting" || data?.phase === "Failed") && data.startup?.logsAvailable !== false);
  const failure = data ? startFailure(data) : null;
  const startup = data?.startup;
  const startupBlocked = startup?.stage === "failed" || startup?.reason === "Unschedulable";

  return (
    <div className="flex flex-col lg:flex-1 lg:min-h-[35rem] gap-4 min-h-0">
      <div>
        <BackLink to="/servers" label={t("my_servers_breadcrumb")} />
      </div>

      {loading && !data ? (
        <Loading />
      ) : error && !data ? (
        <div className="space-y-3"><ErrorState error={error} onRetry={reload} />{isOwner && <HostRecovery name={name} />}</div>
      ) : data ? (
        <>
          {error && <div className="space-y-2"><MessageLine kind="error" message={t("status_stale_detail", { time: updatedAt ? new Date(updatedAt).toLocaleTimeString() : "—", error: humanizeError(error) })} />{isOwner && <HostRecovery name={name} />}</div>}
          <PageHeader
            icon={Terminal}
            title={data.displayName || data.name}
            subtitle={cfg ? <CopyAddress address={joinAddress(data.subdomain, cfg)} /> : undefined}
            actions={
              <div className="flex items-center gap-2">
                {error ? (
                  <span role="status" className="text-sm font-medium text-destructive">{t("status_stale")}</span>
                ) : startupBlocked ? (
                  <Badge variant="outline" className="border-amber-500/30 text-amber-600 dark:text-amber-400">
                    {t(startup?.stage === "failed" ? "startup_problem" : "startup_waiting_resources")}
                  </Badge>
                ) : (
                  <PhaseBadge phase={shownPhase(data)} failure={failure} autoRestarts={data.autoRestarts} />
                )}
                {!error && <PowerButton
                  name={name}
                  phase={data.phase}
                  desiredState={data.desiredState}
                  failed={failure !== null}
                  playersOnline={data.playersOnline}
                  playerCountUnknown={data.playerCountUnknown}
                  retiring={data.retiring}
                  onChanged={reload}
                />}
              </div>
            }
            className="mb-6"
          />

          {data.retiring && (
            <RetireNotice name={name} retiring={data.retiring} isAdmin={isAdmin} onChanged={reload} />
          )}

          {!error && startup && (
            <div role="status" className="shrink-0 space-y-2 rounded-xl border border-border bg-card p-4 text-sm">
              <p className="flex items-center gap-2 font-medium">{startupBlocked ? <ShieldAlert className="h-4 w-4 text-amber-500" /> : <Loader2 className="h-4 w-4 animate-spin text-primary" />}{t(`startup_${startup.stage}`)}</p>
              {startup.startedAt && <p className="text-xs text-muted-foreground">{t("startup_elapsed", { seconds: Math.max(0, Math.floor((Date.now() - new Date(startup.startedAt).getTime()) / 1000)) })}</p>}
              {startup.reason && <p className="break-words text-muted-foreground">{startup.reason === "Unschedulable" && startup.message?.includes("Insufficient memory") ? t("startup_memory") : [startup.reason, startup.message].filter(Boolean).join(": ")}</p>}
              <p className="text-xs text-muted-foreground">{t(startup.stage === "failed" ? "startup_failed_hint" : "startup_hint")}</p>
            </div>
          )}
          <div className="grid grid-cols-1 gap-6 lg:grid-cols-4 lg:grid-rows-[minmax(0,1fr)] flex-1 lg:min-h-0 min-h-0">
            {/* Left/Main column: Console */}
            <div className="lg:col-span-3 flex flex-col lg:min-h-0 min-h-0 h-full">
              <Card className="dark flex flex-col flex-1 lg:min-h-0 min-h-0 overflow-hidden bg-black text-zinc-50 border-zinc-800">
                <CardContent className="p-0 flex-1 flex flex-col lg:min-h-0 min-h-0 bg-black">
                  {!streamable ? (
                    <div className="flex-1 flex flex-col justify-center p-6 bg-black">
                      <NotStreaming phase={error ? "Unknown" : shownPhase(data)} waitingForContainer={!error && data.startup?.logsAvailable === false} />
                    </div>
                  ) : cfg ? (
                    <div className="flex-1 flex flex-col lg:min-h-0 min-h-0 bg-black">
                      {failure && (
                        <FailedStartNotice failure={failure} autoRestarts={data.autoRestarts ?? 0} />
                      )}
                      <LogConsole
                        key={name}
                        starting={!error && data.phase === "Starting"}
                        url={consoleStreamURL(cfg.apiBase, name)}
                        className="border-0 rounded-none bg-transparent"
                      />
                      {!error && data.phase === "Running" && (
                        <div className="p-3 bg-zinc-900/40 border-t border-zinc-800">
                          <CommandInput name={name} />
                        </div>
                      )}
                    </div>
                  ) : (
                    <div className="p-6 bg-black">
                      <Loading />
                    </div>
                  )}
                </CardContent>
              </Card>
            </div>

             {/* Right/Sidebar column: Navigation */}
            <div className="flex flex-col gap-4 lg:col-span-1 lg:min-h-0 lg:overflow-y-auto shrink-0">
              {isAdmin && (
                <EditServerDialog
                  serverName={name}
                  systemService={name === "login" || name === "lobby"}
                  currentDisplayName={data.displayName}
                  currentPolicy={data.autostartPolicy as AutostartPolicy}
                  currentImage={data.image}
                  currentMemory={data.memory}
                  currentStorage={data.storageSize}
                  currentCpu={data.cpu}
                  currentIdleStopSeconds={data.idleStopSeconds}
                  playerCountUnknown={data.playerCountUnknown}
                  onUpdated={reload}
                />
              )}
              {owned && (
                <Link
                  to={`/servers/${name}/luckperms`}
                  className="group flex items-center gap-3 rounded-xl border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
                >
                  <Shield className="h-4 w-4 shrink-0 text-primary" />
                  <div className="min-w-0 flex-1">
                    <p className="text-sm font-medium">{t("luckperms_link_title")}</p>
                    <p className="text-xs text-muted-foreground mt-0.5">
                      {t("luckperms_link_desc")}
                    </p>
                  </div>
                  <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
                </Link>
              )}
              {/* Player management lives on its own subpage (whitelist / online / bans),
                  not crammed under the console. This is the doorway to it; the page
                  itself owns the ownership + readiness gating. */}
              <Link
                to={`/servers/${name}/players`}
                className="group flex items-center gap-3 rounded-xl border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
              >
                <Users className="h-4 w-4 shrink-0 text-primary" />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium">{t("players_link_title")}</p>
                  <p className="text-sm text-muted-foreground">{t("players_link_desc")}</p>
                </div>
                <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
              </Link>

              {/* Backups & restore — its own subpage, doorway mirrors the players card.
                  The page owns its ownership gating and works while the server sleeps (a
                  restore in fact needs it stopped, so this doorway shows at every phase). */}
              <Link
                to={`/servers/${name}/backups`}
                className="group flex items-center gap-3 rounded-xl border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
              >
                <Archive className="h-4 w-4 shrink-0 text-primary" />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium">{t("backups_link_title")}</p>
                  <p className="text-sm text-muted-foreground">{t("backups_link_desc")}</p>
                </div>
                <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
              </Link>

              {/* File editor — its own subpage. Files are only reachable while the server
                  is stopped (the world volume is RWO), so the page itself owns that gate
                  and offers the stop action; the doorway shows at every phase. */}
              <Link
                to={`/servers/${name}/files`}
                className="group flex items-center gap-3 rounded-xl border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
              >
                <FolderOpen className="h-4 w-4 shrink-0 text-primary" />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium">{t("files_link_title")}</p>
                  <p className="text-sm text-muted-foreground">{t("files_link_desc")}</p>
                </div>
                <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
              </Link>

              {/* Scheduled tasks — its own subpage. A schedule runs whatever phase the
                  server is in (a start wakes it, a backup stops it first), so the
                  doorway shows at every phase and the page owns its gating. */}
              <Link
                to={`/servers/${name}/schedules`}
                className="group flex items-center gap-3 rounded-xl border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
              >
                <CalendarClock className="h-4 w-4 shrink-0 text-primary" />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium">{t("schedules_link_title")}</p>
                  <p className="text-sm text-muted-foreground">{t("schedules_link_desc")}</p>
                </div>
                <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
              </Link>

              {/* Giving the server up (owner) or deleting it (admin) closes the
                  sidebar. A system server is never retired, and one already on its
                  way out shows the notice above with the way back instead. */}
              {owned && (isOwner || (!data.reaperExempt && !data.retiring)) && (
                <RetireCard name={name} label={data.displayName || data.name} isAdmin={isAdmin} isOwner={isOwner} canRetire={!data.reaperExempt && !data.retiring} onChanged={reload} />
              )}
            </div>
          </div>
        </>
      ) : null}
    </div>
  );
}

function HostRecovery({ name }: { name: string }) {
  const { t } = useTranslation("servers");
  if (!/^[a-z0-9][a-z0-9-]{0,31}$/.test(name)) return null;
  return <details className="rounded-xl border border-border bg-card p-3 text-sm"><summary className="cursor-pointer font-medium">{t("host_recovery")}</summary><p className="my-3 text-muted-foreground">{t("host_recovery_hint")}</p><pre className="overflow-x-auto whitespace-pre-wrap rounded bg-muted p-3 text-xs">{`kubectl patch minecraftserver ${name} -n minecraft --type=merge -p '{"spec":{"desiredState":"Stopped"}}'
kubectl scale statefulset ${name} -n minecraft --replicas=0
kubectl get pods -n minecraft -l felis.lolicon.best/server=${name},felis.lolicon.best/component=server -o wide`}</pre><p className="mt-3 text-muted-foreground">{t("host_recovery_verify")}</p></details>;
}
