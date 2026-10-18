import { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef, useState } from "react";
import { loadConfig, type RuntimeConfig } from "./config";

/** useConfig resolves the runtime config (apiBase + rootDomain) once on mount.
 *  Returns null until loaded; loadConfig itself never rejects (it falls back). */
export function useConfig(): RuntimeConfig | null {
  const [cfg, setCfg] = useState<RuntimeConfig | null>(null);
  useEffect(() => {
    let alive = true;
    loadConfig().then((c) => {
      if (alive) setCfg(c);
    });
    return () => {
      alive = false;
    };
  }, []);
  return cfg;
}

export interface AsyncState<T> {
  data: T | null;
  error: unknown;
  loading: boolean;
  updatedAt: number | null;
  /** Re-run the async fn (e.g. after a mutation). */
  reload: () => void;
}

export interface AsyncOptions {
  // Retain prior rows while filter or pagination dependencies reload.
  keepPrevious?: boolean;
  /** Polling must not supersede a request that is still awaiting its timeout. */
  coalesce?: boolean;
}

interface Settled<T> {
  /** The producer this result came from; a new one means new deps. */
  run: () => Promise<T>;
  data: T | null;
  error: unknown;
  loading: boolean;
  updatedAt: number | null;
}

// Guard unmounts and stale responses; changed dependencies must never expose another resource's data.
export function useAsync<T>(
  fn: () => Promise<T>,
  deps: unknown[] = [],
  { keepPrevious = false, coalesce = false }: AsyncOptions = {},
): AsyncState<T> {
  const [settled, setSettled] = useState<Settled<T> | null>(null);
  const seq = useRef(0);
  const inFlight = useRef<{ run: () => Promise<T>; ticket: number } | null>(null);

  // eslint-disable-next-line react-hooks/exhaustive-deps
  const run = useCallback(fn, deps);

  const reload = useCallback(() => {
    if (coalesce && inFlight.current?.run === run) return;
    const ticket = ++seq.current;
    inFlight.current = { run, ticket };
    setSettled((s) => ({
      run,
      data: s?.run === run || keepPrevious ? (s?.data ?? null) : null,
      error: s?.run === run ? s.error : null,
      updatedAt: s?.run === run ? s.updatedAt : null,
      loading: true,
    }));
    run().then(
      (d) => {
        if (inFlight.current?.ticket === ticket) inFlight.current = null;
        if (ticket === seq.current) setSettled({ run, data: d, error: null, loading: false, updatedAt: Date.now() });
      },
      (e) => {
        if (inFlight.current?.ticket === ticket) inFlight.current = null;
        if (ticket === seq.current) {
          setSettled((s) => ({ run, data: s?.data ?? null, error: e, loading: false, updatedAt: s?.updatedAt ?? null }));
        }
      },
    );
  }, [run, keepPrevious, coalesce]);

  useEffect(() => {
    reload();
    // Bumping the ticket on cleanup drops any response still in flight.
    const tickets = seq;
    return () => {
      tickets.current++;
      inFlight.current = null;
    };
  }, [reload]);

  // Checked at render: between new deps and the effect that reloads them the
  // state still holds the old producer's result.
  const current = settled?.run === run;
  return {
    data: current || keepPrevious ? (settled?.data ?? null) : null,
    error: current ? settled.error : null,
    loading: current ? settled.loading : true,
    reload,
    updatedAt: current ? settled.updatedAt : null,
  };
}

export function useUnsavedGuard(dirty: boolean): void {
  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      // Older Chromium and Safari need returnValue set to show the prompt.
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);
}

/** How often a server page rereads its status: fast while it waits on the
 *  server (coming up, going down, or needed up), slow while nothing is due. */
export const STATUS_POLL_FAST_MS = 4_000;
export const STATUS_POLL_SLOW_MS = 15_000;

// Poll only while visible and refresh on return; null pauses polling.
export function usePolling(reload: () => void, ms: number | null): void {
  useEffect(() => {
    if (ms === null) return;
    const tick = () => {
      if (!document.hidden) reload();
    };
    const id = window.setInterval(tick, ms);
    document.addEventListener("visibilitychange", tick);
    return () => {
      window.clearInterval(id);
      document.removeEventListener("visibilitychange", tick);
    };
  }, [reload, ms]);
}

// Block page refresh while any card has unsaved changes.
export interface RefreshScope {
  signal: number;
  report: (card: string, dirty: boolean) => void;
}

export const RefreshContext = createContext<RefreshScope | null>(null);

/** usePageRefresh is the page's side: the scope to provide, the trigger for the
 *  header button, and whether any card holds unsaved changes. */
export function usePageRefresh(): { scope: RefreshScope; refresh: () => void; dirty: boolean } {
  const [signal, setSignal] = useState(0);
  const [dirtyCards, setDirtyCards] = useState<ReadonlySet<string>>(() => new Set());
  const report = useCallback((card: string, dirty: boolean) => {
    setDirtyCards((cards) => {
      if (cards.has(card) === dirty) return cards;
      const next = new Set(cards);
      if (dirty) next.add(card);
      else next.delete(card);
      return next;
    });
  }, []);
  const scope = useMemo(() => ({ signal, report }), [signal, report]);
  const refresh = useCallback(() => setSignal((n) => n + 1), []);
  return { scope, refresh, dirty: dirtyCards.size > 0 };
}

// Outside a page refresh scope, this hook does nothing.
export function useRefreshSignal(reload: () => void, dirty = false): void {
  const scope = useContext(RefreshContext);
  const card = useId();
  const report = scope?.report;
  useEffect(() => {
    if (!report) return;
    report(card, dirty);
    return () => report(card, false);
  }, [report, card, dirty]);
  const signal = scope?.signal;
  const seen = useRef(signal);
  useEffect(() => {
    if (signal === undefined || signal === seen.current) return;
    seen.current = signal;
    reload();
  }, [signal, reload]);
}
