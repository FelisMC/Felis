import { useCallback, useEffect, useRef, useSyncExternalStore } from "react";
import {
  LogStreamController,
  type EventSourceFactory,
  type StreamStatus,
  type LogLine,
} from "./logstream";
import { useTier } from "./tier";

// browserEventSource is the production factory: a native EventSource with
// credentials, so the Cloudflare Access cookie rides along exactly as api.ts's
// fetch(credentials:"include") does. EventSource cannot set an Authorization
// header, so cookie auth is what makes a direct SSE attach viable (spec §8).
const browserEventSource: EventSourceFactory = (url) =>
  new EventSource(url, { withCredentials: true });

export interface UseLogStream {
  lines: LogLine[];
  status: StreamStatus;
  retryable: boolean;
  clear: () => void;
  reconnect: () => void;
}

/**
 * useLogStream binds a LogStreamController to React. The controller is the store
 * (useSyncExternalStore); this hook only manages its lifecycle:
 *
 *  - One controller per url. A ref keyed on url keeps a single stable instance
 *    across re-renders (incl. StrictMode's double render) so buffered lines
 *    survive; it is recreated only when url actually changes.
 *  - The effect calls open() on mount and close() on cleanup. That cleanup is
 *    the teardown linchpin: under StrictMode's dev mount→unmount→remount, the
 *    first EventSource is closed before the second opens, so no stream leaks and
 *    lines are never doubled.
 */
export function useLogStream(url: string): UseLogStream {
  const ref = useRef<{ url: string; ctrl: LogStreamController } | null>(null);
  if (ref.current === null || ref.current.url !== url) {
    ref.current = { url, ctrl: new LogStreamController({ url, factory: browserEventSource }) };
  }
  const controller = ref.current.ctrl;

  useEffect(() => {
    controller.open();
    return () => controller.close();
  }, [controller]);

  const snapshot = useSyncExternalStore(controller.subscribe, controller.getSnapshot);

  // EventSource reports a refused stream (a 401 included) only as an error, so
  // an ended stream re-checks the session: an expired one takes the person to
  // sign in, and otherwise the console shows the stream as ended.
  const { revalidate } = useTier();
  useEffect(() => {
    if (snapshot.status === "ended") void revalidate();
  }, [snapshot.status, revalidate]);

  const clear = useCallback(() => controller.clear(), [controller]);
  const reconnect = useCallback(() => controller.reconnect(), [controller]);

  return { lines: snapshot.lines, status: snapshot.status, retryable: snapshot.retryable, clear, reconnect };
}
