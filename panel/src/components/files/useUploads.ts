import { useCallback, useEffect, useRef, useState } from "react";
import i18next from "i18next";
import { api, humanizeError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import type { ServerFileEntry } from "@/lib/types";
import { joinPath } from "./names";

/** The upload ceiling, mirrored from fileedit.MaxUploadBytes (server truth, 413
 *  above it). Checked here so a file too large is refused before it is sent. */
export const MAX_UPLOAD_BYTES = 64 * 1024 * 1024;

/** queued waits its turn; exists found a file already at its path and waits for
 *  replace or skip; failed stays until it is retried or dismissed. */
export type UploadState = "queued" | "uploading" | "done" | "exists" | "failed";

export interface UploadItem {
  id: number;
  file: File;
  /** The folder it lands in, fixed when it was added. */
  dir: string;
  state: UploadState;
  sent: number;
  overwrite: boolean;
  error: string | null;
  /** false for a refusal sending again cannot change (too large, a folder there). */
  retryable: boolean;
}

// useUploads runs a queue of uploads into a server's world volume, one at a time:
// each is a file Job that holds the world lock, so a second one sent alongside
// would only be refused with 409 maintenance_in_progress. onLanded runs after
// each upload that landed, with the folder it landed in.
export function useUploads(server: string, onLanded: (dir: string) => void) {
  const [items, setItems] = useState<UploadItem[]>([]);
  const nextId = useRef(1);
  const running = useRef<number | null>(null);
  const abort = useRef<AbortController | null>(null);
  const landed = useRef(onLanded);
  landed.current = onLanded;

  const patch = useCallback((id: number, change: Partial<UploadItem>) => {
    setItems((all) => all.map((it) => (it.id === id ? { ...it, ...change } : it)));
  }, []);
  const drop = useCallback((id: number) => {
    setItems((all) => all.filter((it) => it.id !== id));
  }, []);

  // Leaving the page stops the upload in flight; the queued ones were never sent.
  useEffect(() => () => abort.current?.abort(), []);

  useEffect(() => {
    if (running.current !== null) return;
    const next = items.find((it) => it.state === "queued");
    if (!next) return;
    running.current = next.id;
    const ctrl = new AbortController();
    abort.current = ctrl;
    patch(next.id, { state: "uploading", sent: 0, error: null });
    const settle = () => {
      running.current = null;
      abort.current = null;
    };
    api
      .uploadServerFile(server, joinPath(next.dir, next.file.name), next.file, next.overwrite, {
        signal: ctrl.signal,
        onProgress: (sent) => patch(next.id, { sent }),
      })
      .then(
        () => {
          settle();
          patch(next.id, { state: "done", sent: next.file.size });
          landed.current(next.dir);
        },
        (e: unknown) => {
          settle();
          if (e instanceof DOMException && e.name === "AbortError") {
            drop(next.id);
          } else if ((e as { code?: string }).code === "file_exists") {
            // Someone put a file there since the listing: ask, as for one listed.
            patch(next.id, { state: "exists", sent: 0 });
          } else {
            patch(next.id, { state: "failed", sent: 0, error: humanizeError(e), retryable: true });
          }
        },
      );
  }, [items, server, patch, drop]);

  /** add queues files for dir. `entries` is dir's listing: a file of the same
   *  name there waits for replace or skip instead of being sent to be refused. */
  const add = useCallback((files: readonly File[], dir: string, entries: readonly ServerFileEntry[] | null) => {
    const t = i18next.getFixedT(null, "files");
    const added = files.map((file): UploadItem => {
      const base = { id: nextId.current++, file, dir, sent: 0, overwrite: false, error: null, retryable: false };
      const there = entries?.find((e) => e.name === file.name);
      if (file.size > MAX_UPLOAD_BYTES) {
        return { ...base, state: "failed", error: t("upload_too_large", { limit: formatBytes(MAX_UPLOAD_BYTES) }) };
      }
      if (there?.is_dir) {
        return { ...base, state: "failed", error: t("upload_folder_there") };
      }
      return { ...base, state: there ? "exists" : "queued" };
    });
    setItems((all) => [...all, ...added]);
  }, []);

  const replace = useCallback((id: number) => patch(id, { state: "queued", overwrite: true }), [patch]);
  const retry = useCallback((id: number) => patch(id, { state: "queued", error: null }), [patch]);
  /** remove cancels an upload in flight, or takes any other one off the list. */
  const remove = useCallback(
    (id: number) => {
      if (running.current === id) abort.current?.abort();
      else drop(id);
    },
    [drop],
  );
  const replaceAll = useCallback(() => {
    setItems((all) => all.map((it) => (it.state === "exists" ? { ...it, state: "queued", overwrite: true } : it)));
  }, []);
  const skipAll = useCallback(() => setItems((all) => all.filter((it) => it.state !== "exists")), []);
  const clearDone = useCallback(() => setItems((all) => all.filter((it) => it.state !== "done")), []);

  const busy = items.some((it) => it.state === "queued" || it.state === "uploading");
  return { items, busy, add, replace, retry, remove, replaceAll, skipAll, clearDone };
}
