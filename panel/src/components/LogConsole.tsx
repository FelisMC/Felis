import { useCallback, useLayoutEffect, useRef, useState } from "react";
import { ArrowDown, RotateCw, Trash2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useLogStream } from "@/lib/useLogStream";
import type { LogLevel, StreamStatus } from "@/lib/logstream";

// Per-level tint. Plain/info are the default readable foreground; warn/error draw
// the eye. Debug is dimmed so it recedes. The console body is a fixed dark
// surface so it reads as a terminal regardless of the page theme.
const LEVEL_CLASS: Record<LogLevel, string> = {
  error: "text-red-400",
  warn: "text-amber-300",
  info: "text-zinc-200",
  debug: "text-zinc-500",
  plain: "text-zinc-300",
};

const PIN_THRESHOLD_PX = 24;

function StatusIndicator({ status }: { status: StreamStatus }) {
  const { t } = useTranslation("servers");
  const map: Record<StreamStatus, { dot: string; label: string; text: string }> = {
    connecting: { dot: "bg-amber-400 animate-pulse", label: t("log_connecting"), text: "text-amber-300" },
    open: { dot: "bg-emerald-400", label: t("log_live"), text: "text-emerald-300" },
    reconnecting: { dot: "bg-amber-400 animate-pulse", label: t("log_reconnecting"), text: "text-amber-300" },
    ended: { dot: "bg-zinc-500", label: t("log_ended"), text: "text-zinc-400" },
  };
  const s = map[status];
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs font-medium", s.text)}>
      <span className={cn("h-2 w-2 rounded-full", s.dot)} />
      {s.label}
    </span>
  );
}

/**
 * LogConsole renders the §8 read-side live console: a one-way SSE feed of the
 * running pod's log. It is intentionally NOT a terminal emulator (xterm) — the
 * feed is read-only, so a purpose-built log viewport is the right tool. Writes
 * go through the separate RCON command path; the panel never holds an RCON
 * password (spec §8/§286).
 *
 * UX follows modern log viewers: it follows the tail, but if you scroll up to
 * read history it stops yanking you down and offers a "Jump to latest" pill;
 * scrolling back to the bottom re-pins. The buffer is bounded by the controller.
 */
export function LogConsole({ url }: { url: string }) {
  const { t } = useTranslation("servers");
  const { lines, status, clear, reconnect } = useLogStream(url);
  const scrollRef = useRef<HTMLDivElement>(null);
  const [pinned, setPinned] = useState(true);

  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
    setPinned(distance <= PIN_THRESHOLD_PX);
  }, []);

  // Follow the tail only while pinned. Runs after layout so the scroll lands on
  // the just-appended line; when the user has scrolled up (pinned=false) it does
  // nothing, leaving their position untouched.
  useLayoutEffect(() => {
    if (!pinned) return;
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [lines, pinned]);

  const jumpToLatest = useCallback(() => setPinned(true), []);

  return (
    <div className="overflow-hidden rounded-md border border-border bg-zinc-950">
      {/* Toolbar */}
      <div className="flex items-center justify-between gap-2 border-b border-zinc-800 bg-zinc-900/60 px-3 py-2">
        <StatusIndicator status={status} />
        <div className="flex items-center gap-1.5">
          {status === "ended" && (
            <Button
              variant="outline"
              size="sm"
              onClick={reconnect}
              className="border-zinc-700 bg-transparent text-zinc-200 hover:bg-zinc-800 hover:text-zinc-50"
            >
              <RotateCw className="h-3.5 w-3.5" /> {t("log_reconnect_btn")}
            </Button>
          )}
          <Button
            variant="ghost"
            size="sm"
            onClick={clear}
            disabled={lines.length === 0}
            className="text-zinc-300 hover:bg-zinc-800 hover:text-zinc-50"
          >
            <Trash2 className="h-3.5 w-3.5" /> {t("log_clear_btn")}
          </Button>
        </div>
      </div>

      {/* Viewport */}
      <div className="relative">
        <div
          ref={scrollRef}
          onScroll={onScroll}
          className="h-[28rem] overflow-y-auto px-3 py-2 font-mono text-xs leading-relaxed"
        >
          {lines.length === 0 ? (
            <p className="select-none py-8 text-center text-zinc-600">
              {status === "ended"
                ? t("log_ended_empty")
                : t("log_waiting")}
            </p>
          ) : (
            lines.map((line) => (
              <div key={line.seq} className={cn("whitespace-pre-wrap break-all", LEVEL_CLASS[line.level])}>
                {line.text || "\u00A0"}
              </div>
            ))
          )}
        </div>

        {/* Jump-to-latest pill, shown only when the user has scrolled away from
            the tail. */}
        {!pinned && (
          <button
            type="button"
            onClick={jumpToLatest}
            className="absolute bottom-3 left-1/2 inline-flex -translate-x-1/2 items-center gap-1.5 rounded-full bg-primary px-3 py-1 text-xs font-medium text-primary-foreground shadow-lg transition-colors hover:bg-primary/90"
          >
            <ArrowDown className="h-3.5 w-3.5" /> {t("log_jump_latest")}
          </button>
        )}
      </div>
    </div>
  );
}
