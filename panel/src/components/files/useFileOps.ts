import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";
import { usePolling } from "@/lib/hooks";
import type { FileOp } from "@/lib/types";
import { OP_POLL_MS } from "./sessionUpload";

// useFileOps follows a server's background ops (an extraction, or the landing of
// a file sent in parts): read once when `enabled` turns on, then again every
// OP_POLL_MS while one runs. They carry on with the page closed, so a visit
// after one began still shows it and how it ended. onEnded runs once for each op
// seen running here that has since ended.
export function useFileOps(server: string, enabled: boolean, onEnded: (op: FileOp) => void) {
  const [ops, setOps] = useState<FileOp[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [dismissed, setDismissed] = useState<ReadonlySet<string>>(new Set());
  const seenRunning = useRef(new Set<string>());
  const ended = useRef(onEnded);
  ended.current = onEnded;
  // Only the newest read lands, and a read is not started over one in flight:
  // a slow answer must not undo a newer one, nor pile up behind the interval.
  const seq = useRef(0);
  const inFlight = useRef(false);
  // Ops the upload queue follows itself (the landing of a file it sent in
  // parts): its own row shows them, so they are left out here.
  const ignored = useRef(new Set<string>());

  const read = useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    const ticket = ++seq.current;
    try {
      const r = await api.listServerFileOps(server);
      if (ticket !== seq.current) return;
      const all = (r.ops ?? []).filter((op) => !ignored.current.has(op.id));
      setError(null);
      setOps(all);
      for (const op of all) {
        if (op.state === "running") seenRunning.current.add(op.id);
        else if (seenRunning.current.delete(op.id)) ended.current(op);
      }
    } catch (e) {
      if (ticket === seq.current) setError(e);
    } finally {
      inFlight.current = false;
    }
  }, [server]);

  useEffect(() => {
    if (enabled) void read();
  }, [enabled, read]);

  const running = ops.some((op) => op.state === "running");
  const poll = useCallback(() => void read(), [read]);
  usePolling(poll, enabled && running ? OP_POLL_MS : null);

  /** started shows an op this page just began at once, ahead of the next read,
   *  and watches it from there. */
  const started = useCallback((op: FileOp) => {
    if (op.state === "running") seenRunning.current.add(op.id);
    seq.current++; // a read already in flight predates it
    setOps((all) => [op, ...all.filter((o) => o.id !== op.id)]);
  }, []);

  const dismiss = useCallback((id: string) => setDismissed((s) => new Set(s).add(id)), []);
  /** ignore leaves the op id out from now on. */
  const ignore = useCallback((id: string) => {
    ignored.current.add(id);
    setOps((all) => all.filter((o) => o.id !== id));
  }, []);

  return { ops: ops.filter((op) => !dismissed.has(op.id)), running, error, refresh: poll, started, dismiss, ignore };
}
