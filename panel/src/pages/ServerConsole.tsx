import { useState, useRef, useCallback, useEffect, useId, useLayoutEffect, type KeyboardEvent } from "react";
import { Link, useParams } from "react-router-dom";
import { Terminal, Moon, Shield, ShieldAlert, HelpCircle, Loader2, SendHorizontal, Users, Archive, FolderOpen, Puzzle, CalendarClock, ChevronRight, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { BackLink } from "@/components/BackLink";
import { MAX_AUTO_RESTARTS, PhaseBadge, pendingPower, shownPhase, startFailure, type StartFailure } from "@/components/PhaseBadge";
import { PageHeader } from "@/components/PageHeader";
import { FormattedText, LogConsole } from "@/components/LogConsole";
import { ConsoleHistory } from "@/components/ConsoleHistory";
import { Loading, ErrorState } from "@/components/States";
import { api, consoleStreamURL, humanizeError } from "@/lib/api";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS, useAsync, useConfig, usePolling } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage } from "@/lib/ownership";
import { joinAddress } from "@/lib/config";
import { CopyAddress } from "@/components/CopyAddress";
import type { Phase, AutostartPolicy } from "@/lib/types";
import { EditServerDialog } from "@/components/EditServerDialog";
import { SoftwareCard } from "@/components/SoftwareCard";
import { ResourceUsage } from "@/components/ResourceUsage";
import { ForwardingDialog } from "@/components/ForwardingDialog";
import { PowerButton } from "@/components/PowerButton";
import { RetireCard, RetireNotice } from "@/components/Retirement";
import { RecoveryNotice, ServerEventsCard } from "@/components/ServerEvents";
import { parseFormatting, type Formatted } from "@/lib/mcformat";
import { cn } from "@/lib/utils";
import { InlineError, MessageLine } from "@/components/MessageLine";

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

function CommandInput({ name }: { name: string }) {
  const { t } = useTranslation("servers");
  const [command, setCommand] = useState("");
  const [sending, setSending] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [last, setLast] = useState<{ cmd: string; state: "pending" | "rcon" | "stdin" | "failed"; out?: Formatted } | null>(null);
  const inFlight = useRef(false);
  const generation = useRef(0);
  const historyRef = useRef<string[]>(loadHistory(name));
  const cursorRef = useRef(-1);
  // What was typed before stepping up into the history.
  const draftRef = useRef("");
  const inputRef = useRef<HTMLInputElement>(null);
  const composingRef = useRef(false);
  useEffect(() => () => { generation.current += 1; inFlight.current = false; }, []);

  const send = useCallback(async (text: string) => {
    if (!text || inFlight.current) return;
    inFlight.current = true;
    const ticket = ++generation.current;
    setSending(true);
    setErr(null);
    setLast({ cmd: text, state: "pending" });
    const h = historyRef.current;
    if (h.length === 0 || h[h.length - 1] !== text) {
      if (h.length >= MAX_HISTORY) h.shift();
      h.push(text);
      persistHistory(name, h);
    }
    cursorRef.current = h.length;
    try {
      const res = await api.sendCommand(name, text);
      if (ticket !== generation.current) return;
      setCommand("");
      setLast({ cmd: text, state: res.transport, out: res.transport === "rcon" ? parseFormatting(res.output.trim()) : undefined });
    } catch (ex) {
      if (ticket !== generation.current) return;
      setLast({ cmd: text, state: "failed" });
      setErr((ex as { code?: string }).code === "not_running" ? t("command_not_running") : humanizeError(ex));
    } finally {
      if (ticket === generation.current) { inFlight.current = false; setSending(false); }
    }
  }, [name, t]);

  useLayoutEffect(() => {
    if (!sending) inputRef.current?.focus();
  }, [sending]);

  const onKeyDown = useCallback((e: KeyboardEvent<HTMLInputElement>) => {
    if (composingRef.current) return;
    const h = historyRef.current;
    if (e.key === "Enter") {
      e.preventDefault();
      if (!e.repeat) void send(command.trim());
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
          {last.state === "pending" && <span role="status">{t("command_sending")}</span>}
          {last.state === "stdin" && <span role="status">{t("command_sent_stdin")}</span>}
          {last.out && <FormattedText formatted={last.out} />}
        </pre>
      )}
      <div className="flex items-center gap-2 rounded-md border border-zinc-700 bg-zinc-900 px-3 font-mono text-xs text-zinc-200">
        <span className="shrink-0 select-none text-zinc-500">{">"}</span>
        <input
          ref={inputRef}
          value={command}
          disabled={sending}
          onChange={(e) => { setCommand(e.target.value); cursorRef.current = -1; }}
          onKeyDown={onKeyDown}
          onCompositionStart={() => { composingRef.current = true; }}
          onCompositionEnd={() => { composingRef.current = false; }}
          aria-label={t("command_label")}
          placeholder={t("command_placeholder")}
          autoComplete="off"
          enterKeyHint="send"
          spellCheck={false}
          className="min-w-0 flex-1 bg-transparent py-2 text-base text-zinc-200 placeholder:text-zinc-500 focus:outline-none sm:text-xs"
        />
        <Button type="button" size="icon" variant="ghost" aria-label={t("command_send")} title={t("command_send")}
          disabled={sending || !command.trim()} onClick={() => void send(command.trim())}
          className="h-9 w-9 shrink-0 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100">
          {sending ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <SendHorizontal className="h-3.5 w-3.5" />}
        </Button>
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
  // Poll faster during power transitions and retry backoff.
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
  const savedLogs = owned && !error && data?.desiredState === "Stopped" && !data.ready && !["Running", "Starting", "Stopping"].includes(data.phase);
  const commandable = owned && !error && data?.desiredState === "Running" && ["Starting", "Running"].includes(data.phase);

  // Stream Starting and Failed Pods as well as Running Pods so startup failures remain diagnosable.
  const streamable = data?.phase === "Running" || ((data?.phase === "Starting" || data?.phase === "Failed") && data.startup?.logsAvailable !== false);
  const failure = data ? startFailure(data) : null;
  const startup = data?.startup;
  const startupBlocked = startup?.stage === "failed" || startup?.reason === "Unschedulable";

  return (
    <div className="flex shrink-0 flex-col lg:flex-1 lg:shrink lg:min-h-[35rem] gap-4 min-h-0">
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
              <div className="flex min-w-0 max-w-full flex-wrap items-center justify-end gap-2">
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
                  diskFull={data.diskFull}
                  allowRestart={owned}
                  onChanged={reload}
                />}
              </div>
            }
            className="mb-6"
          />

          {data.retiring && (
            <RetireNotice name={name} retiring={data.retiring} isAdmin={isAdmin} onChanged={reload} />
          )}
          {!error && <RecoveryNotice data={data} />}

          {owned && <ResourceUsage key={name} name={name} running={!error && data.phase === "Running" && data.desiredState === "Running"} cpu={data.cpu} memory={data.memory} disk={error && data.disk ? { ...data.disk, stale: true } : data.disk} diskFull={data.diskFull} />}
          {!error && startup && (
            <div role="status" className="shrink-0 space-y-2 rounded-xl border border-border bg-card p-4 text-sm">
              <p className="flex items-center gap-2 font-medium">{startupBlocked ? <ShieldAlert className="h-4 w-4 text-amber-500" /> : <Loader2 className="h-4 w-4 animate-spin text-primary" />}{t(`startup_${startup.stage}`)}</p>
              {startup.startedAt && <p className="text-xs text-muted-foreground">{t("startup_elapsed", { seconds: Math.max(0, Math.floor((Date.now() - new Date(startup.startedAt).getTime()) / 1000)) })}</p>}
              {startup.reason && <p className="break-words text-muted-foreground">{startup.reason === "Unschedulable" && startup.message?.includes("Insufficient memory") ? t("startup_memory") : [startup.reason, startup.message].filter(Boolean).join(": ")}</p>}
              <p className="text-xs text-muted-foreground">{t(startup.stage === "failed" ? "startup_failed_hint" : "startup_hint")}</p>
            </div>
          )}
          <div className="grid grid-cols-1 gap-6 lg:grid-cols-4 lg:grid-rows-[minmax(0,1fr)] lg:flex-1 lg:min-h-0">
            {/* Left/Main column: Console */}
            <div className="lg:col-span-3 flex flex-col lg:min-h-0 lg:h-full">
              <Card className="dark flex flex-col lg:flex-1 lg:min-h-0 overflow-hidden bg-black text-zinc-50 border-zinc-800">
                <CardContent className="p-0 lg:flex-1 flex flex-col lg:min-h-0 bg-black">
                  {savedLogs ? (
                    <ConsoleHistory key={name} name={name} />
                  ) : !streamable ? (
                    <div className="flex-1 flex flex-col justify-center p-6 bg-black">
                      <NotStreaming phase={error ? "Unknown" : shownPhase(data)} waitingForContainer={!error && data.startup?.logsAvailable === false} />
                    </div>
                  ) : cfg ? (
                    <div className="h-[55vh] flex flex-col lg:h-auto lg:flex-1 lg:min-h-0 bg-black">
                      {failure && (
                        <FailedStartNotice failure={failure} autoRestarts={data.autoRestarts ?? 0} />
                      )}
                      <LogConsole
                        key={name}
                        starting={!error && data.phase === "Starting"}
                        url={consoleStreamURL(cfg.apiBase, name)}
                        className="border-0 rounded-none bg-transparent"
                      />
                    </div>
                  ) : (
                    <div className="p-6 bg-black">
                      <Loading />
                    </div>
                  )}
                  {commandable && <div className="shrink-0 border-t border-zinc-800 bg-zinc-900/40 p-3">
                    <CommandInput key={name} name={name} />
                  </div>}
                </CardContent>
              </Card>
            </div>

             {/* Right/Sidebar column: Navigation */}
            <div className="flex flex-col gap-4 lg:col-span-1 lg:min-h-0 lg:overflow-y-auto shrink-0">
              {data.software && <SoftwareCard software={data.software} />}
              {(isAdmin || owned) && (
                <EditServerDialog
                  serverName={name}
                  currentPhase={data.phase}
                  mode={isAdmin ? "admin" : "owner"}
                  systemService={name === "login" || name === "lobby"}
                  currentDisplayName={data.displayName}
                  currentPolicy={data.autostartPolicy as AutostartPolicy}
                  currentImage={data.image}
                  currentMemory={data.memory}
                  currentStorage={data.storageSize}
                  currentCpu={data.cpu}
                  currentIdleStopSeconds={data.idleStopSeconds}
                  playerCountUnknown={data.playerCountUnknown}
                  currentSoftware={data.software}
                  currentResume={data.resumeAfterHostRestart}
                  currentCrashRestart={data.restartOnCrash}
                  currentMaxCrashRestarts={data.maxCrashRestarts}
                  currentNotify={data.notify}
                  onUpdated={reload}
                />
              )}
              {isOwner && name !== "login" && name !== "lobby" && <ForwardingDialog serverName={name} legacy={!!data.legacyForwarding} onUpdated={reload} />}
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

              {/* The file page permits running or stopped servers and owns its access checks. */}
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

              {owned && <Link
                to={`/servers/${name}/extensions`}
                className="group flex items-center gap-3 rounded-xl border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
              >
                <Puzzle className="h-4 w-4 shrink-0 text-primary" />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium">{t("extensions_title")}</p>
                  <p className="text-sm text-muted-foreground">{t("extensions_link_desc")}</p>
                </div>
                <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
              </Link>}

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

              {data.events && data.events.length > 0 && <ServerEventsCard events={data.events} />}

              {/* Giving the server up (owner) or deleting it (admin) closes the
                  sidebar. A system server is never retired, and one already on its
                  way out shows the notice above with the way back instead. */}
              {owned && (isOwner || data.phase !== "Stopped" || (!data.reaperExempt && !data.retiring)) && (
                <RetireCard name={name} label={data.displayName || data.name} isAdmin={isAdmin} isOwner={isOwner} canKill={!error && (data.phase !== "Stopped" || data.desiredState === "Running")} canRetire={!data.reaperExempt && !data.retiring} onChanged={reload} />
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
