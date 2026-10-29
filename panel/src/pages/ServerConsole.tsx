import { useState, useRef, useCallback, useEffect, useId, useLayoutEffect, type KeyboardEvent } from "react";
import { Moon, ShieldAlert, HelpCircle, Loader2, SendHorizontal, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { MAX_AUTO_RESTARTS, shownPhase, startFailure, type StartFailure } from "@/components/PhaseBadge";
import { FormattedText, LogConsole } from "@/components/LogConsole";
import { ConsoleHistory } from "@/components/ConsoleHistory";
import { Loading } from "@/components/States";
import { api, consoleStreamURL, humanizeError } from "@/lib/api";
import { useConfig } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import type { Phase, ServerStatus } from "@/lib/types";
import { formatDuration } from "@/lib/format";
import { ResourceUsage } from "@/components/ResourceUsage";
import { RetireNotice } from "@/components/Retirement";
import { RecoveryNotice, ServerEventsCard } from "@/components/ServerEvents";
import { parseFormatting, rconLines, type Formatted } from "@/lib/mcformat";
import { cn } from "@/lib/utils";
import { InlineError } from "@/components/MessageLine";
import { useServer } from "@/pages/ServerLayout";

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
    <div className="flex max-w-md flex-col items-center gap-2 text-sm text-zinc-500">
      <Icon className={cn("h-5 w-5 text-zinc-600", spin && "animate-spin")} />
      <p className="font-medium text-zinc-300">{title}</p>
      <p>{body}</p>
    </div>
  );
}

// StartupStrip heads the console while a start is in progress, so the reason it waits sits next to where its logs will appear.
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
      setLast({ cmd: text, state: res.transport, out: res.transport === "rcon" ? parseFormatting(rconLines(res.output.trim())) : undefined });
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
  const { name, data, error, owned, reload } = useServer();
  const cfg = useConfig();
  const { isAdmin } = useTier();
  const { t } = useTranslation("servers");
  const savedLogs = owned && !error && data.desiredState === "Stopped" && !data.ready && !["Running", "Starting", "Stopping"].includes(data.phase);
  const commandable = owned && !error && data.desiredState === "Running" && ["Starting", "Running"].includes(data.phase);
  // Stream Starting and Failed Pods as well as Running Pods so startup failures remain diagnosable.
  const streamable = data.phase === "Running" || ((data.phase === "Starting" || data.phase === "Failed") && data.startup?.logsAvailable !== false);
  const failure = startFailure(data);
  const startup = data.startup;
  const startupBlocked = startup?.stage === "failed" || startup?.reason === "Unschedulable";

  return (
    <div className="flex flex-col gap-4">
      {data.retiring && (
        <RetireNotice name={name} retiring={data.retiring} isAdmin={isAdmin} onChanged={reload} />
      )}
      {!error && <RecoveryNotice data={data} />}
      <Card className="dark flex h-[clamp(24rem,68dvh,52rem)] flex-col overflow-hidden border-zinc-800 bg-black text-zinc-50">
        <CardContent className="flex min-h-0 flex-1 flex-col bg-black p-0">
          {!error && startup && <StartupStrip startup={startup} blocked={startupBlocked} />}
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
            <div className="flex min-h-0 flex-1 flex-col bg-black">
              {failure && <FailedStartNotice failure={failure} autoRestarts={data.autoRestarts ?? 0} />}
              <LogConsole
                key={name}
                starting={!error && data.phase === "Starting"}
                url={consoleStreamURL(cfg.apiBase, name)}
                className="rounded-none border-0 bg-transparent"
              />
            </div>
          ) : (
            <div className="bg-black p-6"><Loading /></div>
          )}
          {commandable && (
            <div className="shrink-0 border-t border-zinc-800 bg-zinc-900/40 p-3">
              <CommandInput key={name} name={name} />
            </div>
          )}
        </CardContent>
      </Card>

      {owned && <ResourceUsage key={name} name={name} running={!error && data.phase === "Running" && data.desiredState === "Running"} cpu={data.cpu} memory={data.memory} disk={error && data.disk ? { ...data.disk, stale: true } : data.disk} diskFull={data.diskFull} />}
      {data.events && data.events.length > 0 && <ServerEventsCard events={data.events} />}
    </div>
  );
}
