import { useState, useRef, useCallback, useLayoutEffect, type KeyboardEvent } from "react";
import { Link, useParams } from "react-router-dom";
import { Terminal, Moon, Shield, ShieldAlert, HelpCircle, Loader2, Users, Archive, FolderOpen, ChevronRight, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Card, CardContent } from "@/components/ui/card";
import { BackLink } from "@/components/BackLink";
import { PhaseBadge } from "@/components/PhaseBadge";
import { PageHeader } from "@/components/PageHeader";
import { LogConsole } from "@/components/LogConsole";
import { Loading, ErrorState } from "@/components/States";
import { api, consoleStreamURL, humanizeError } from "@/lib/api";
import { useAsync, useConfig } from "@/lib/hooks";
import { useTier } from "@/lib/tier";
import { canManage } from "@/lib/ownership";
import { hostFor } from "@/lib/config";
import type { Phase, AutostartPolicy } from "@/lib/types";
import { EditServerDialog } from "@/components/EditServerDialog";
import { PowerButton } from "@/components/PowerButton";
import { InlineError } from "@/components/MessageLine";

// notStreamingCopy explains why there is no live feed for a phase that has no
// streamable pod. The read path only has something to relay once a pod is up, so
// Stopped/Failed/Stopping each get their own honest line rather than an empty
// console. `default` covers Unknown plus any future phase the backend may emit
// that the panel hasn't modelled yet — the screen stays informative regardless.
function useNotStreamingCopy(phase: Phase): { icon: LucideIcon; title: string; body: string } {
  const { t } = useTranslation("servers");
  switch (phase) {
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
    case "Failed":
      return {
        icon: ShieldAlert,
        title: t("server_failed_title"),
        body: t("server_failed_body"),
      };
    default:
      return {
        icon: HelpCircle,
        title: t("console_offline_title"),
        body: t("console_offline_body"),
      };
  }
}

function NotStreaming({ phase }: { phase: Phase }) {
  const { icon: Icon, title, body } = useNotStreamingCopy(phase);
  return (
    <div className="flex items-start gap-3 rounded-md border border-dashed border-border bg-muted/30 p-6 text-sm text-muted-foreground">
      <Icon className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground/70" />
      <div className="space-y-1">
        <p className="font-medium text-foreground">{title}</p>
        <p>{body}</p>
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
 *  sends an RCON command to the running server and displays its plain-text reply.
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
  const [last, setLast] = useState<{ cmd: string; out: string } | null>(null);
  const historyRef = useRef<string[]>(loadHistory(name));
  const cursorRef = useRef(-1);
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
      setLast({ cmd: text, out: (res.output ?? "").trim() });
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
    if (e.key === "ArrowUp") {
      e.preventDefault();
      const idx = Math.max(0, cursorRef.current === -1 ? h.length - 1 : cursorRef.current - 1);
      cursorRef.current = idx;
      setCommand(h[idx]);
      setTimeout(() => { inputRef.current?.setSelectionRange(h[idx].length, h[idx].length); }, 0);
    }
    if (e.key === "ArrowDown") {
      e.preventDefault();
      const idx = Math.min(h.length, cursorRef.current + 1);
      cursorRef.current = idx;
      setCommand(idx < h.length ? h[idx] : "");
      if (idx < h.length) {
        setTimeout(() => { inputRef.current?.setSelectionRange(h[idx].length, h[idx].length); }, 0);
      }
    }
  }, [command, send]);

  return (
    <div className="space-y-1.5">
      {last && (
        <pre className="max-h-40 overflow-y-auto whitespace-pre-wrap break-words rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2 font-mono text-xs text-zinc-300">
          <span className="text-zinc-500">{"> " + last.cmd + "\n"}</span>
          {last.out}
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
  const { isAdmin } = useTier();
  const { t } = useTranslation("servers");
  const { data, error, loading, reload } = useAsync(
    () => api.status(name),
    [name],
  );
  const { data: mine } = useAsync(
    () => (isAdmin ? Promise.resolve([]) : api.myServers()),
    [isAdmin, name],
  );
  const owned = canManage(isAdmin, mine, name);

  // The read-side stream (spec §8) follows the running pod's log. A pod exists
  // during BOTH Starting and Running — the operator marks Starting once the pod
  // is up but RCON is not yet reachable (it only flips to Running after RCON
  // readiness). Boot logs flow precisely in that Starting window, which is when a
  // read most wants them, so the gate streams for both, not Running alone.
  const streamable = data?.phase === "Running" || data?.phase === "Starting";

  return (
    <div className="flex flex-col lg:h-[calc(100vh-3.5rem)] lg:min-h-[35rem] gap-4 min-h-0">
      <div>
        <BackLink to="/servers" label={t("my_servers_breadcrumb")} />
      </div>

      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorState error={error} onRetry={reload} />
      ) : data ? (
        <>
          <PageHeader
            icon={Terminal}
            title={data.displayName || data.name}
            subtitle={cfg ? hostFor(data.subdomain, cfg) : undefined}
            actions={
              <div className="flex items-center gap-2">
                <PhaseBadge phase={data.phase} />
                <PowerButton
                  name={name}
                  live={streamable || data.phase === "Stopping"}
                  playersOnline={data.playersOnline}
                  playerCountUnknown={data.playerCountUnknown}
                  onChanged={reload}
                />
              </div>
            }
            className="mb-6"
          />

          <div className="grid grid-cols-1 gap-6 lg:grid-cols-4 flex-1 lg:min-h-0 min-h-0">
            {/* Left/Main column: Console */}
            <div className="lg:col-span-3 flex flex-col lg:min-h-0 min-h-0 h-full">
              <Card className="dark flex flex-col flex-1 lg:min-h-0 min-h-0 overflow-hidden bg-black text-zinc-50 border-zinc-800">
                <CardContent className="p-0 flex-1 flex flex-col lg:min-h-0 min-h-0 bg-black">
                  {!streamable ? (
                    <div className="flex-1 flex flex-col justify-center p-6 bg-black">
                      <NotStreaming phase={data.phase} />
                    </div>
                  ) : cfg ? (
                    <div className="flex-1 flex flex-col lg:min-h-0 min-h-0 bg-black">
                      <LogConsole
                        key={name}
                        url={consoleStreamURL(cfg.apiBase, name)}
                        className="border-0 rounded-none bg-transparent"
                      />
                      {data.phase === "Running" && (
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
            <div className="flex flex-col gap-4 lg:col-span-1 shrink-0">
              {isAdmin && (
                <EditServerDialog
                  serverName={name}
                  currentDisplayName={data.displayName}
                  currentPolicy={data.autostartPolicy as AutostartPolicy}
                  currentImage={data.image}
                  currentMemory={data.javaMemory}
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
                  className="group flex items-center gap-3 rounded-lg border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
                >
                  <Shield className="h-5 w-5 shrink-0 text-primary" />
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
                className="group flex items-center gap-3 rounded-lg border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
              >
                <Users className="h-5 w-5 shrink-0 text-primary" />
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
                className="group flex items-center gap-3 rounded-lg border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
              >
                <Archive className="h-5 w-5 shrink-0 text-primary" />
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
                className="group flex items-center gap-3 rounded-lg border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-accent"
              >
                <FolderOpen className="h-5 w-5 shrink-0 text-primary" />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium">{t("files_link_title")}</p>
                  <p className="text-sm text-muted-foreground">{t("files_link_desc")}</p>
                </div>
                <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5" />
              </Link>
            </div>
          </div>
        </>
      ) : null}
    </div>
  );
}
