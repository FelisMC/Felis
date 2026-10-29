import { memo, useEffect, useCallback, useLayoutEffect, useMemo, useRef, useState, type CSSProperties } from "react";
import { ArrowDown, RotateCw, Trash2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useLogStream } from "@/lib/useLogStream";
import { chunkLines, sameChunk } from "@/lib/logchunks";
import type { LogLevel, LogLine, LogWriter, StreamStatus } from "@/lib/logstream";
import type { Formatted, Segment } from "@/lib/mcformat";

const LEVEL_CLASS: Record<LogLevel, string> = {
  error: "text-red-400",
  warn: "text-amber-300",
  info: "text-zinc-200",
  debug: "text-zinc-500",
  plain: "text-zinc-300",
};

const PIN_THRESHOLD_PX = 24;

function segmentStyle(s: Segment): CSSProperties | undefined {
  const decoration = [s.underline && "underline", s.strike && "line-through"].filter(Boolean).join(" ");
  if (!s.color && !s.bold && !s.italic && !decoration) return undefined;
  return {
    color: s.color,
    fontWeight: s.bold ? 700 : undefined,
    fontStyle: s.italic ? "italic" : undefined,
    textDecorationLine: decoration || undefined,
  };
}

export function FormattedText({ formatted }: { formatted: Formatted }) {
  return formatted.segments
    ? formatted.segments.map((segment, i) => <span key={i} style={segmentStyle(segment)}>{segment.text}</span>)
    : formatted.text;
}

export function LogRow({ line }: { line: Pick<LogLine, "text" | "segments" | "level" | "input"> }) {
  return <div className={cn("whitespace-pre-wrap break-all", line.input ? "text-sky-300" : LEVEL_CLASS[line.level])}>
    <FormattedText formatted={line.text ? line : { text: "\u00A0" }} />
  </div>;
}

// Match only INFO lifecycle messages; RCON failures and command output remain visible.
export const RCON_LIFECYCLE = /^(?:\[\d{2}:\d{2}:\d{2} INFO\]|\[\d{2}:\d{2}:\d{2}(?:\.\d+)?\] \[[^\]]+\/INFO\]): Thread RCON Client \/\S+ (?:started|shutting down)$/;

// A chunk off screen is sized from its last layout, or before it has had one,
// from 100 unwrapped lines of text-xs at leading-relaxed (19.5px each).
const LogChunk = memo(function LogChunk({ lines }: { lines: LogLine[] }) {
  return (
    <div className="[content-visibility:auto] [contain-intrinsic-size:auto_1950px]">
      {lines.map((line) => (
        <LogRow key={line.seq} line={line} />
      ))}
    </div>
  );
}, sameChunk);

export function RconToggle({ shown, hidden, onToggle }: { shown: boolean; hidden: number; onToggle: () => void }) {
  const { t } = useTranslation("servers");
  if (hidden === 0 && !shown) return null;
  return (
    <Button
      variant="ghost"
      size="sm"
      aria-pressed={shown}
      onClick={onToggle}
      className="text-zinc-300 hover:bg-zinc-800 hover:text-zinc-50"
    >
      {t(shown ? "log_hide_rcon" : "log_show_rcon")}
      {hidden > 0 && <span className="tabular-nums">({hidden})</span>}
    </Button>
  );
}

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

// Logs are read-only SSE; onWriter hands out a way to echo typed commands into it.
// Pause tail following during history inspection and resume at the bottom.
export function LogConsole({ url, className, starting = false, onWriter }: { url: string; className?: string; starting?: boolean; onWriter?: (write: LogWriter | null) => void }) {
  const { t } = useTranslation("servers");
  const { lines, status, retryable, clear, reconnect, write } = useLogStream(url);
  useEffect(() => {
    if (!onWriter) return;
    onWriter(write);
    return () => onWriter(null);
  }, [onWriter, write]);
  useEffect(() => {
    if (!starting || !retryable || status !== "ended") return;
    const timer = window.setTimeout(reconnect, 5000);
    return () => window.clearTimeout(timer);
  }, [starting, retryable, status, reconnect]);
  const [showRcon, setShowRcon] = useState(false);
  const visibleLines = useMemo(() => showRcon ? lines : lines.filter((line) => !RCON_LIFECYCLE.test(line.text)), [lines, showRcon]);
  const hiddenCount = lines.length - visibleLines.length;
  const chunks = useMemo(() => chunkLines(visibleLines), [visibleLines]);
  const scrollRef = useRef<HTMLDivElement>(null);
  const [pinned, setPinned] = useState(true);

  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
    setPinned(distance <= PIN_THRESHOLD_PX);
  }, []);

  // Scroll after layout only while pinned; preserve position during history inspection.
  useLayoutEffect(() => {
    if (!pinned) return;
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [visibleLines, pinned]);

  const jumpToLatest = useCallback(() => setPinned(true), []);

  return (
    <div className={cn("overflow-hidden rounded-md border border-border bg-black flex-1 flex flex-col min-h-0", className)}>
      {/* Toolbar */}
      <div className="flex items-center justify-between gap-2 border-b border-zinc-800 bg-zinc-900/60 px-3 py-2 shrink-0">
        <StatusIndicator status={starting && retryable && status === "ended" ? "reconnecting" : status} />
        <div className="flex items-center gap-1.5">
          <RconToggle shown={showRcon} hidden={hiddenCount} onToggle={() => setShowRcon((shown) => !shown)} />
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
      <div className="relative flex-1 min-h-0">
        <div
          ref={scrollRef}
          onScroll={onScroll}
          className="h-full overflow-y-auto px-3 py-2 font-mono text-xs leading-relaxed"
        >
          {visibleLines.length === 0 ? (
            <p className="select-none py-8 text-center text-zinc-600">
              {status === "ended" && (!starting || !retryable)
                ? t("log_ended_empty")
                : t(hiddenCount > 0 ? "log_rcon_only" : starting ? "log_starting_wait" : "log_waiting")}
            </p>
          ) : (
            chunks.map((chunk) => <LogChunk key={chunk.key} lines={chunk.lines} />)
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
