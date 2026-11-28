import { useState, useRef, useCallback, useEffect, useId, useLayoutEffect, type KeyboardEvent } from "react";
import { Lock, Moon, ShieldAlert, HelpCircle, Loader2, SendHorizontal, Snowflake, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { MAX_AUTO_RESTARTS, PhaseBadge, shownPhase, startFailure, type StartFailure } from "@/components/PhaseBadge";
import { FormattedText, LogConsole } from "@/components/LogConsole";
import { ConsoleHistory } from "@/components/ConsoleHistory";
import { Loading } from "@/components/States";
import { api, consoleStreamURL, humanizeError } from "@/lib/api";
import { useConfig } from "@/lib/hooks";
import { canDeleteServer, isReadonlyRole } from "@/lib/ownership";
import { useTier } from "@/lib/tier";
import type { Phase, ServerHibernation, ServerStatus } from "@/lib/types";
import { formatBytes, formatDuration } from "@/lib/format";
import { cpuUsage, memoryUsage } from "@/lib/resourceUsage";
import { RetireNotice } from "@/components/Retirement";
import { RecoveryNotice } from "@/components/ServerEvents";
import { parseFormatting, rconLines, type Formatted } from "@/lib/mcformat";
import type { LogWriter } from "@/lib/logstream";
import { cn } from "@/lib/utils";
import { InlineError } from "@/components/MessageLine";
import { useServer } from "@/pages/ServerLayout";

function parseCpuMilli(limit?: string): number | null {
  if (!limit) return null;
  const s = limit.trim();
  if (s.endsWith("m")) {
    const n = Number(s.slice(0, -1));
    return Number.isFinite(n) && n > 0 ? n : null;
  }
  const n = Number(s);
  return Number.isFinite(n) && n > 0 ? n * 1000 : null;
}

function parseMemBytes(limit?: string): number | null {
  if (!limit) return null;
  const m = /^(\d+(?:\.\d+)?)\s*(Gi|Mi|Ki|G|M|K)?i?B?$/i.exec(limit.trim());
  if (!m) return null;
  const val = Number(m[1]);
  const unit = (m[2] ?? "").toLowerCase();
  const mult = unit === "gi" || unit === "g" ? 1024 ** 3 : unit === "mi" || unit === "m" ? 1024 ** 2 : unit === "ki" || unit === "k" ? 1024 : 1;
  return Number.isFinite(val) && val > 0 ? val * mult : null;
}

function LiveTelemetryStrip({ data }: { data: ServerStatus }) {
  const { t } = useTranslation("servers");
  const cpuText = cpuUsage(data.cpuMilli, data.cpu);
  const memText = memoryUsage(data.memoryBytes, data.memory);
  const diskText = data.disk ? `${formatBytes(data.disk.usedBytes)} / ${formatBytes(data.disk.limitBytes)}` : "—";
  const playersText =
    data.phase === "Running" && !data.playerCountUnknown ? `${data.playersOnline} / ${data.playersMax}` : "—";

  const cpuLimit = parseCpuMilli(data.cpu);
  const cpuPct = data.cpuMilli != null && cpuLimit ? Math.min(100, Math.round((data.cpuMilli / cpuLimit) * 100)) : 0;
  const memLimit = parseMemBytes(data.memory);
  const memPct = data.memoryBytes != null && memLimit ? Math.min(100, Math.round((data.memoryBytes / memLimit) * 100)) : 0;
  const diskPct = data.disk && data.disk.limitBytes > 0 ? Math.min(100, Math.round((data.disk.usedBytes / data.disk.limitBytes) * 100)) : 0;
  const playerPct = data.phase === "Running" && data.playersMax > 0 ? Math.min(100, Math.round((data.playersOnline / data.playersMax) * 100)) : 0;

  const cells = [
    { key: "cpu", label: t("usage_cpu"), value: cpuText, pct: cpuPct, accent: true },
    { key: "mem", label: t("usage_memory"), value: memText, pct: memPct, accent: false },
    { key: "disk", label: t("disk_title"), value: diskText, pct: diskPct, accent: false },
    { key: "players", label: t("tab_players"), value: playersText, pct: playerPct, accent: true },
  ];

  return (
    <div className="grid shrink-0 grid-cols-2 divide-y divide-border/60 overflow-hidden rounded-lg border border-border bg-card shadow-[0_1px_2px_rgba(0,0,0,0.03)] sm:grid-cols-4 sm:divide-x sm:divide-y-0 dark:shadow-none">
      {cells.map(({ key, label, value, pct, accent }) => (
        <div key={key} className="px-3.5 py-2.5">
          <div className="mb-1.5 flex items-center justify-between gap-2 text-[11px] text-muted-foreground">
            <span className="truncate">{label}</span>
            <span className="shrink-0 font-mono font-medium text-foreground tabular-nums">{value}</span>
          </div>
          <div className="h-[2px] w-full overflow-hidden rounded-full bg-secondary">
            <div
              className={cn("h-full rounded-full transition-all", accent ? "bg-primary" : "bg-foreground/35")}
              style={{ width: `${pct}%` }}
            />
          </div>
        </div>
      ))}
    </div>
  );
}

function useNotStreamingCopy(phase: Phase, waitingForContainer: boolean): { icon: LucideIcon; title: string; body: string; spin?: boolean } {
  const { t } = useTranslation("servers");
  if (waitingForContainer) return { icon: HelpCircle, title: t("logs_unavailable_title"), body: t("logs_unavailable_body") };
  switch (phase) {
    case "Starting":
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
    <div className="flex max-w-md flex-col items-center gap-2 text-sm text-zinc-500">
      <Icon className={cn("h-5 w-5 text-zinc-600", spin && "animate-spin")} />
      <p className="font-medium text-zinc-300">{title}</p>
      <p>{body}</p>
    </div>
  );
}

function StartupStrip({ startup, blocked }: { startup: NonNullable<ServerStatus["startup"]>; blocked: boolean }) {
  const { t } = useTranslation("servers");
  const waited = startup.startedAt ? Math.max(0, Math.floor((Date.now() - new Date(startup.startedAt).getTime()) / 1000)) : null;
  return (
    <div role="status" className={cn("shrink-0 space-y-1 border-b border-zinc-800 bg-zinc-900/70 px-4 py-3 text-sm text-zinc-300", blocked && "border-l-2 border-l-amber-500")}>
      <p className="flex flex-wrap items-center gap-x-2 gap-y-0.5 font-medium text-zinc-100">
        {blocked ? <ShieldAlert className="h-4 w-4 text-amber-400" /> : <Loader2 className="h-4 w-4 animate-spin text-primary" />}
        {t(`startup_${startup.stage}`)}
        {waited !== null && <span className="text-xs font-normal text-zinc-500">{t("startup_elapsed", { duration: formatDuration(waited, t) })}</span>}
      </p>
      {startup.reason && <p className="break-words">{startup.reason === "Unschedulable" && startup.message?.includes("Insufficient memory") ? t("startup_memory") : [startup.reason, startup.message].filter(Boolean).join(": ")}</p>}
      <p className="text-xs text-zinc-500">{t(startup.stage === "failed" ? "startup_failed_hint" : "startup_hint")}</p>
    </div>
  );
}

function HibernationStrip({ hibernation, waking }: { hibernation: ServerHibernation; waking: boolean }) {
  const { t } = useTranslation("servers");
  const blocked = !!hibernation.message;
  const frozen = hibernation.stage === "Frozen" && !waking;
  const kind = hibernation.stage === "Freezing" ? "freezing" : frozen ? "frozen" : blocked ? "wake_blocked" : "waking";
  const Icon = blocked ? ShieldAlert : frozen ? Snowflake : Loader2;
  return (
    <div role="status" className={cn("shrink-0 space-y-1 border-b border-zinc-800 bg-zinc-900/70 px-4 py-3 text-sm text-zinc-300", blocked && "border-l-2 border-l-amber-500")}>
      <p className="flex items-center gap-2 font-medium text-zinc-100">
        <Icon className={cn("h-4 w-4", blocked ? "text-amber-400" : frozen ? "text-indigo-300" : "animate-spin text-primary")} />
        {t(`hibernation_${kind}_title`)}
      </p>
      <p>{t(`hibernation_${kind}_body`, { size: formatBytes(hibernation.swappedBytes ?? 0) })}</p>
      {blocked && <p className="break-words text-xs text-zinc-500">{hibernation.message}</p>}
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

type Echo = (raw: string, input?: boolean) => boolean;

function CommandInput({ name, echo }: { name: string; echo: Echo }) {
  const { t } = useTranslation("servers");
  const [command, setCommand] = useState("");
  const [sending, setSending] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [note, setNote] = useState<Formatted | null>(null);
  const inFlight = useRef(false);
  const generation = useRef(0);
  const historyRef = useRef<string[]>(loadHistory(name));
  const cursorRef = useRef(-1);
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
    setNote(null);
    const shown = echo("> " + text, true);
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
      const reply = res.transport === "rcon" ? rconLines(res.output.trim()) : "";
      if (!shown) setNote(reply ? parseFormatting(reply) : { text: t("command_sent_stdin") });
      else if (reply) for (const line of reply.split("\n")) echo(line);
    } catch (ex) {
      if (ticket !== generation.current) return;
      setErr((ex as { code?: string }).code === "not_running" ? t("command_not_running") : humanizeError(ex));
    } finally {
      if (ticket === generation.current) { inFlight.current = false; setSending(false); }
    }
  }, [name, t, echo]);

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
    const onLine = cursorRef.current === -1 || cursorRef.current >= h.length;
    if (e.key === "ArrowUp") {
      e.preventDefault();
      if (onLine) draftRef.current = command;
      const idx = Math.max(0, onLine ? h.length - 1 : cursorRef.current - 1);
      cursorRef.current = idx;
      setCommand(h[idx]);
      setTimeout(() => { inputRef.current?.setSelectionRange(h[idx].length, h[idx].length); }, 0);
    }
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
      {note && (
        <pre role="status" className="max-h-40 overflow-y-auto whitespace-pre-wrap break-words font-mono text-xs text-zinc-400">
          <FormattedText formatted={note} />
        </pre>
      )}
      <div className="flex items-center gap-2 font-mono text-xs text-zinc-200">
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
          className="min-w-0 flex-1 bg-transparent py-1 text-base text-zinc-100 placeholder:text-zinc-500 focus:outline-none sm:text-xs"
        />
        <Button
          type="button"
          size="icon"
          variant="outline"
          aria-label={t("command_send")}
          title={t("command_send")}
          disabled={sending || !command.trim()}
          onClick={() => void send(command.trim())}
          className="h-6 w-6 shrink-0 border-zinc-700 bg-zinc-800/80 text-zinc-200 hover:bg-zinc-700 hover:text-white"
        >
          {sending ? <Loader2 className="h-3 w-3 animate-spin" /> : <SendHorizontal className="h-3 w-3" />}
        </Button>
      </div>
      <InlineError message={err} className="text-xs" />
    </div>
  );
}

export function ServerConsole() {
  const { name, data, error, accessRole, canOperate, actualOwned, reload } = useServer();
  const writer = useRef<LogWriter | null>(null);
  const onWriter = useCallback((write: LogWriter | null) => { writer.current = write; }, []);
  const echo = useCallback<Echo>((raw, input) => {
    if (!writer.current) return false;
    writer.current(raw, input);
    return true;
  }, []);
  const cfg = useConfig();
  const { isAdmin, isOwner } = useTier();
  const { t } = useTranslation("servers");
  const savedLogs = canOperate && !error && data.desiredState === "Stopped" && !data.ready && !["Running", "Starting", "Stopping"].includes(data.phase);
  const commandable = canOperate && !error && data.desiredState === "Running" && ["Starting", "Running"].includes(data.phase);
  const streamable = canOperate && (data.phase === "Running" || data.phase === "Hibernating" || data.phase === "Hibernated" ||
    ((data.phase === "Starting" || data.phase === "Failed") && data.startup?.logsAvailable !== false));
  const failure = startFailure(data);
  const startup = data.startup;
  const startupBlocked = startup?.stage === "failed" || startup?.reason === "Unschedulable";
  const showTelemetry = canOperate && !error && (data.phase === "Running" || data.cpuMilli != null || data.memoryBytes != null || data.disk != null);
  const canCancelRetire = data.retiring
    ? data.retiring.delete
      ? canDeleteServer(isOwner, isAdmin, data.protected)
      : !data.protected && (isOwner || isAdmin || actualOwned)
    : false;

  if (isReadonlyRole(accessRole) || (!!data.protected && !canOperate)) {
    return (
      <div className="flex flex-1 flex-col gap-3.5">
        {data.retiring && (
          <RetireNotice name={name} retiring={data.retiring} isAdmin={isAdmin} canCancel={canCancelRetire} onChanged={reload} />
        )}
        <Card>
          <CardContent className="flex flex-col items-start gap-3 py-6 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex items-start gap-3">
              <Lock className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" aria-hidden="true" />
              <div className="space-y-1">
                <p className="text-sm font-medium text-foreground">{t("readonly_console_title")}</p>
                <p className="text-xs text-muted-foreground">{t("readonly_console_body")}</p>
              </div>
            </div>
            <PhaseBadge phase={error ? "Unknown" : shownPhase(data)} failure={failure} autoRestarts={data.autoRestarts} />
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-3.5">
      {data.retiring && (
        <RetireNotice name={name} retiring={data.retiring} isAdmin={isAdmin} canCancel={canCancelRetire} onChanged={reload} />
      )}
      {!error && <RecoveryNotice data={data} />}
      {showTelemetry && <LiveTelemetryStrip data={data} />}
      <Card className="dark flex min-h-96 flex-1 flex-col overflow-hidden border-zinc-800 bg-[#09090b] text-zinc-50">
        <CardContent className="flex min-h-0 flex-1 flex-col bg-[#09090b] p-0">
          {!error && data.hibernation && data.hibernation.stage !== "Killing" && <HibernationStrip hibernation={data.hibernation} waking={data.desiredState === "Running"} />}
          {!error && startup && !data.hibernation && <StartupStrip startup={startup} blocked={startupBlocked} />}
          {savedLogs ? (
            <div className="flex min-h-0 flex-1 flex-col"><ConsoleHistory key={name} name={name} /></div>
          ) : !streamable ? (
            <div className="flex flex-1 items-center justify-center px-6 text-center">
              {!error && startup ? (
                <p className="text-sm text-zinc-500">{t("console_waiting_logs")}</p>
              ) : (
                <NotStreaming phase={error ? "Unknown" : shownPhase(data)} waitingForContainer={!error && data.startup?.logsAvailable === false} />
              )}
            </div>
          ) : cfg ? (
            <div className="flex min-h-0 flex-1 flex-col bg-[#09090b]">
              {failure && <FailedStartNotice failure={failure} autoRestarts={data.autoRestarts ?? 0} />}
              <LogConsole
                key={name}
                starting={!error && data.phase === "Starting"}
                url={consoleStreamURL(cfg.apiBase, name)}
                className="rounded-none border-0 bg-transparent"
                onWriter={onWriter}
              />
            </div>
          ) : (
            <div className="bg-[#09090b] p-6"><Loading /></div>
          )}
          {commandable && (
            <div className="shrink-0 border-t border-zinc-800/90 bg-[#121215] p-2.5">
              <CommandInput key={name} name={name} echo={echo} />
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
