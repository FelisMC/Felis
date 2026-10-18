import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";
import { usePolling } from "@/lib/hooks";
import type { ServerJob } from "@/lib/types";
import { OP_POLL_MS } from "./sessionUpload";

// Safety snapshots hold the world until restore starts; backup exports hold only the archive store.
export function holdsWorld(j: ServerJob): boolean {
  return j.then_restore === "pending" || (j.state === "running" && j.kind !== "export_backup");
}

/** restores says whether the holder is (or leads to) a restore, which replaces
 *  the files the page lists. */
function restores(j: ServerJob): boolean {
  return j.kind === "restore" || j.then_restore === "pending";
}

/** holderText is the files key naming what holds the world and what to wait
 *  for. */
export function holderText(j: ServerJob): string {
  if (restores(j)) return "wait_for_restore";
  switch (j.kind) {
    case "backup":
      return "wait_for_backup";
    case "export_world":
      return "wait_for_world_export";
    default:
      return "wait_for_file_download";
  }
}

// Poll other world holders and invoke onRestored when an observed restore finishes.
export function useWorldJobs(server: string, enabled: boolean, onRestored: () => void) {
  const [holder, setHolder] = useState<ServerJob | null>(null);
  const restoring = useRef(false);
  const restored = useRef(onRestored);
  restored.current = onRestored;
  // As in useFileOps: only the newest read lands, and none starts over another.
  const seq = useRef(0);
  const inFlight = useRef(false);

  const read = useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    const ticket = ++seq.current;
    try {
      const jobs = await api.serverJobs(server);
      if (ticket !== seq.current) return;
      const h = jobs.find(holdsWorld) ?? null;
      setHolder(h);
      const now = h !== null && restores(h);
      if (restoring.current && !now) restored.current();
      restoring.current = now;
    } catch {
      // A list that fails leaves the page as it was: a change the world cannot
      // take is still refused, in the server's words.
    } finally {
      inFlight.current = false;
    }
  }, [server]);

  useEffect(() => {
    if (enabled) void read();
    else setHolder(null);
  }, [enabled, read]);

  const poll = useCallback(() => void read(), [read]);
  usePolling(poll, enabled && holder !== null ? OP_POLL_MS : null);

  return { holder, refresh: poll };
}
