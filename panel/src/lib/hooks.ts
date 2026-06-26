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
  /** Re-run the async fn (e.g. after a mutation). */
  reload: () => void;
}

/** useAsync runs an async producer on mount and on demand, guarding against
 *  setState-after-unmount and out-of-order responses. */
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[] = []): AsyncState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);
  const seq = useRef(0);

  // eslint-disable-next-line react-hooks/exhaustive-deps
  const run = useCallback(fn, deps);

  const reload = useCallback(() => {
    const ticket = ++seq.current;
    setLoading(true);
    setError(null);
    run().then(
      (d) => {
        if (ticket === seq.current) {
          setData(d);
          setLoading(false);
        }
      },
      (e) => {
        if (ticket === seq.current) {
          setError(e);
          setLoading(false);
        }
      },
    );
  }, [run]);

  useEffect(() => {
    reload();
    return () => {
      seq.current++;
    };
  }, [reload]);

  return { data, error, loading, reload };
}
