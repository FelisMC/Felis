import { useCallback, useEffect, useRef, useSyncExternalStore } from "react";
import {
  LogStreamController,
  type EventSourceFactory,
  type StreamStatus,
  type LogLine,
} from "./logstream";
import { useTier } from "./tier";

// EventSource needs withCredentials for session-cookie authentication.
const browserEventSource: EventSourceFactory = (url) =>
  new EventSource(url, { withCredentials: true });

export interface UseLogStream {
  lines: LogLine[];
  status: StreamStatus;
  retryable: boolean;
  clear: () => void;
  reconnect: () => void;
}

// Keep one controller per URL and close it during effect cleanup, including StrictMode remounts.
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

  // A refused EventSource exposes no HTTP status, so recheck the session when the stream ends.
  const { revalidate } = useTier();
  useEffect(() => {
    if (snapshot.status === "ended") void revalidate();
  }, [snapshot.status, revalidate]);

  const clear = useCallback(() => controller.clear(), [controller]);
  const reconnect = useCallback(() => controller.reconnect(), [controller]);

  return { lines: snapshot.lines, status: snapshot.status, retryable: snapshot.retryable, clear, reconnect };
}
