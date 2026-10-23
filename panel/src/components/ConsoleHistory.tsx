import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Loader2, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { MessageLine } from "@/components/MessageLine";
import { LogRow } from "@/components/LogConsole";
import { api, humanizeError } from "@/lib/api";
import { classifyLogLine } from "@/lib/logstream";
import { parseFormatting } from "@/lib/mcformat";
import type { ConsoleHistoryPage } from "@/lib/types";

export function ConsoleHistory({ name }: { name: string }) {
  const { t } = useTranslation("servers");
  const [data, setData] = useState<{ bytes: Uint8Array; page: ConsoleHistoryPage } | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(false);
  const generation = useRef(0);
  const busy = useRef(false);
  const lastCursor = useRef<string | undefined>(undefined);
  const viewport = useRef<HTMLDivElement>(null);
  const scroll = useRef<{ height: number; top: number } | "bottom" | null>(null);

  const load = useCallback(async (cursor?: string) => {
    if (busy.current) return;
    busy.current = true;
    lastCursor.current = cursor;
    const ticket = ++generation.current;
    setLoading(true);
    setError(null);
    try {
      const page = await api.serverConsoleHistory(name, cursor);
      if (ticket !== generation.current) return;
      const bytes = Uint8Array.from(atob(page.content), (char) => char.charCodeAt(0));
      const el = viewport.current;
      scroll.current = cursor && el ? { height: el.scrollHeight, top: el.scrollTop } : "bottom";
      setData((previous) => {
        if (!cursor || !previous) return { bytes, page };
        const merged = new Uint8Array(bytes.length + previous.bytes.length);
        merged.set(bytes);
        merged.set(previous.bytes, bytes.length);
        return { bytes: merged, page };
      });
    } catch (reason) {
      if (ticket === generation.current) setError(reason);
    } finally {
      if (ticket === generation.current) {
        busy.current = false;
        setLoading(false);
      }
    }
  }, [name]);

  useEffect(() => {
    setData(null);
    void load();
    return () => { generation.current += 1; busy.current = false; };
  }, [load]);

  useLayoutEffect(() => {
    const el = viewport.current;
    const anchor = scroll.current;
    if (!el || !anchor) return;
    el.scrollTop = anchor === "bottom" ? el.scrollHeight : el.scrollHeight - anchor.height + anchor.top;
    scroll.current = null;
  }, [data]);

  const lines = useMemo(() => {
    if (!data?.bytes.length) return [];
    const text = new TextDecoder().decode(data.bytes);
    const raw = text.split(/\r\n|\r|\n/);
    if (/[\r\n]$/.test(text)) raw.pop();
    return raw.map((line) => {
      const formatted = parseFormatting(line);
      return { ...formatted, level: classifyLogLine(formatted.text) };
    });
  }, [data]);
  const changed = (error as { code?: string } | null)?.code === "file_changed";

  return (
    <section aria-label={t("log_history_title")} className="flex min-h-0 flex-1 flex-col bg-black text-zinc-200">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-zinc-800 bg-zinc-900/60 px-3 py-2">
        <div className="min-w-0 text-xs"><span className="font-medium">{t("log_history_title")}</span><code className="ml-2 text-zinc-400">logs/latest.log</code></div>
        <Button size="sm" variant="ghost" onClick={() => void load()} disabled={loading}
          className="text-zinc-300 hover:bg-zinc-800 hover:text-zinc-50">
          <RefreshCw className="h-3.5 w-3.5" />{t("log_history_refresh")}
        </Button>
      </div>
      {error !== null && <div className="space-y-2 px-3 pt-3">
        <MessageLine kind="error" message={changed ? t("log_history_changed") : t("log_history_failed", { reason: humanizeError(error) })} />
        {!changed && <Button size="sm" variant="outline" disabled={loading} onClick={() => void load(lastCursor.current)}>{t("log_history_retry")}</Button>}
      </div>}
      {data?.page.byte_limited && <p className="px-3 pt-3 text-xs text-amber-300">{t("log_history_byte_limit")}</p>}
      <div ref={viewport} role="region" aria-label={t("log_history_content")} tabIndex={0}
        className="h-[55vh] min-h-48 overflow-y-auto px-3 py-2 font-mono text-xs leading-relaxed lg:h-0 lg:flex-1">
        {data?.page.has_more && !error && <div className="mb-3 flex justify-center">
          <Button size="sm" variant="outline" disabled={loading} onClick={() => void load(data.page.next_cursor)}
            className="border-zinc-700 bg-transparent text-zinc-200 hover:bg-zinc-800 hover:text-zinc-50">
            {loading && <Loader2 className="h-3.5 w-3.5 animate-spin" />}{t("log_history_more")}
          </Button>
        </div>}
        {loading && !data && <p role="status" className="flex items-center justify-center gap-2 py-8 text-zinc-400"><Loader2 className="h-4 w-4 animate-spin" />{t("log_history_loading")}</p>}
        {data && lines.length === 0 && <p className="py-8 text-center text-zinc-500">{t("log_history_empty")}</p>}
        {lines.map((line, index) => <LogRow key={index - lines.length} line={line} />)}
      </div>
    </section>
  );
}
