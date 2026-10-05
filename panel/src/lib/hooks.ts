import { useCallback, useEffect, useRef, useState } from "react";
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
  /** Keep showing the last result while new deps load, for a list whose deps
   *  are its filter or page: the old rows stay put (each still acts on its own
   *  item) instead of blanking to a spinner on every keystroke. */
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

/** useAsync runs an async producer on mount and on demand, guarding against
 *  setState-after-unmount and out-of-order responses. A reload with the same
 *  deps (polling, after a mutation) keeps the current data while it runs. New
 *  deps start from nothing: the result of /servers/a is never shown as
 *  /servers/b, whose buttons already act on b, not even for the one render
 *  before the new request starts. */
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

/** useUnsavedGuard asks the browser to confirm leaving the page (reload, tab
 *  close, typing another URL) while `dirty` holds, so unsaved edits are not
 *  dropped without a prompt. */
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

/** usePolling re-runs `reload` every `ms` while the tab is visible, and at once
 *  when the tab comes back into view, so a page watching a server follows it
 *  without a manual refresh. `ms` null pauses it. */
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
