import { useCallback, useEffect, useRef, useState } from "react";
import i18next from "i18next";
import { api, humanizeError } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import type { FileOpError, ServerFileEntry } from "@/lib/types";
import { joinPath, nameTooLong } from "./names";
import { opErrorText } from "./opText";
import { discardSession, forgetSession, sendInParts, watchOp } from "./sessionUpload";

/** The most one upload request carries, mirrored from fileedit.MaxUploadBytes
 *  (413 above it). A larger file goes up in parts instead (sessionUpload.ts),
 *  with no ceiling but the room on the world volume. */
export const ONE_REQUEST_BYTES = 64 * 1024 * 1024;

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
  /** false for a refusal sending again cannot change (a folder there). */
  retryable: boolean;
  /** How far the Job landing a file sent in parts has got, once it reports. */
  landing: { done: number; total: number } | null;
}

/** An op that ended failed, thrown so the queue settles it like any refusal. */
interface OpFailure {
  status: 0;
  code: string;
  message: string;
  opError: FileOpError;
}

interface Options {
  /** Runs after each upload that landed, with the folder it landed in. */
  onLanded: (dir: string) => void;
  /** Runs with the id of each op a file sent in parts is landed by. */
  onOp?: (id: string) => void;
  /** Holds the queue: nothing new starts while it is true. */
  hold?: boolean;
  /** Bytes free on the world volume, from the latest listing. */
  free?: number | null;
}

// useUploads runs a queue of uploads into a server's world volume, one at a time:
// each is a file Job that holds the world lock, so a second one sent alongside
// would only be refused with 409 maintenance_in_progress.
export function useUploads(server: string, { onLanded, onOp, hold = false, free = null }: Options) {
  const [items, setItems] = useState<UploadItem[]>([]);
  const current = useRef(items);
  current.current = items;
  const nextId = useRef(1);
  const running = useRef<number | null>(null);
  const abort = useRef<AbortController | null>(null);
  // An upload cancelled on purpose gives its session back; one stopped because
  // the page went away keeps it for a resume.
  const cancelled = useRef<number | null>(null);
  // The session each file sent in parts went up in, by item.
  const sessions = useRef(new Map<number, { path: string; id: string }>());
  const hooks = useRef({ onLanded, onOp });
  hooks.current = { onLanded, onOp };
  const room = useRef(free);
  room.current = free;

  const patch = useCallback((id: number, change: Partial<UploadItem>) => {
    setItems((all) => all.map((it) => (it.id === id ? { ...it, ...change } : it)));
  }, []);
  const drop = useCallback((id: number) => {
    setItems((all) => all.filter((it) => it.id !== id));
  }, []);
  // discard gives back the session an item holds, if any.
  const discard = useCallback(
    (id: number) => {
      const s = sessions.current.get(id);
      sessions.current.delete(id);
      if (s) void discardSession(server, s.path, s.id);
    },
    [server],
  );

  // Leaving the page stops the upload in flight; the queued ones were never sent.
  useEffect(() => () => abort.current?.abort(), []);

  // sendLarge sends a file in parts and follows the op landing it to its end.
  const sendLarge = useCallback(
    async (it: UploadItem, path: string, signal: AbortSignal) => {
      const op = await sendInParts(server, path, it.file, {
        overwrite: it.overwrite,
        signal,
        onProgress: (sent) => patch(it.id, { sent }),
        onSession: (id) => sessions.current.set(it.id, { path, id }),
      });
      hooks.current.onOp?.(op.id);
      patch(it.id, { sent: it.file.size });
      const end =
        op.state === "running"
          ? await watchOp(server, op.id, {
              signal,
              onProgress: (o) => patch(it.id, { landing: { done: o.done, total: o.total } }),
            })
          : op;
      if (end.state === "failed") {
        const opError = end.error ?? { code: "job_failed", message: "" };
        const failure: OpFailure = { status: 0, code: opError.code, message: opError.message, opError };
        throw failure;
      }
      sessions.current.delete(it.id);
      forgetSession(server, path);
    },
    [server, patch],
  );

  useEffect(() => {
    if (hold || running.current !== null) return;
    const next = items.find((it) => it.state === "queued");
    if (!next) return;
    running.current = next.id;
    const ctrl = new AbortController();
    abort.current = ctrl;
    patch(next.id, { state: "uploading", sent: 0, error: null, landing: null });
    const settle = () => {
      running.current = null;
      abort.current = null;
    };
    const path = joinPath(next.dir, next.file.name);
    const sending =
      next.file.size > ONE_REQUEST_BYTES
        ? sendLarge(next, path, ctrl.signal)
        : api.uploadServerFile(server, path, next.file, next.overwrite, {
            signal: ctrl.signal,
            onProgress: (sent) => patch(next.id, { sent }),
          });
    sending.then(
      () => {
        settle();
        patch(next.id, { state: "done", sent: next.file.size });
        hooks.current.onLanded(next.dir);
      },
      (e: unknown) => {
        settle();
        if (e instanceof DOMException && e.name === "AbortError") {
          if (cancelled.current === next.id) discard(next.id);
          cancelled.current = null;
          drop(next.id);
        } else if ((e as { code?: string }).code === "file_exists") {
          // Someone put a file there since the listing: ask, as for one listed.
          // A file sent in parts keeps its session, so replacing it lands the
          // bytes already sent.
          patch(next.id, { state: "exists", sent: 0, landing: null });
        } else {
          const opError = (e as Partial<OpFailure>).opError;
          patch(next.id, {
            state: "failed",
            sent: 0,
            landing: null,
            error: opError ? opErrorText("upload", opError) : humanizeError(e),
            retryable: true,
          });
        }
      },
    );
  }, [items, hold, server, patch, drop, discard, sendLarge]);

  /** add queues files for dir. `entries` is dir's listing: a file of the same
   *  name there waits for replace or skip instead of being sent to be refused.
   *  Files the world volume has no room for are refused before any is sent,
   *  counting the ones ahead of them in the same batch. */
  const add = useCallback((files: readonly File[], dir: string, entries: readonly ServerFileEntry[] | null) => {
    const t = i18next.getFixedT(null, "files");
    let budget = room.current;
    const added = files.map((file): UploadItem => {
      const base = {
        id: nextId.current++,
        file,
        dir,
        sent: 0,
        overwrite: false,
        error: null,
        retryable: false,
        landing: null,
      };
      // The server would refuse it only after the bytes were sent.
      if (nameTooLong(file.name)) {
        return { ...base, state: "failed", error: t("upload_name_too_long") };
      }
      const there = entries?.find((e) => e.name === file.name);
      if (there?.is_dir) {
        return { ...base, state: "failed", error: t("upload_folder_there") };
      }
      if (budget !== null && file.size > budget) {
        return { ...base, state: "failed", error: noRoom(file, budget), retryable: true };
      }
      if (budget !== null) budget -= file.size;
      return { ...base, state: there ? "exists" : "queued" };
    });
    setItems((all) => [...all, ...added]);
  }, []);

  // requeue sends the items picked again once the volume has room for each by
  // the latest listing; one it has none for stays failed and says so again.
  const requeue = useCallback((pick: (it: UploadItem) => boolean, change: Partial<UploadItem>) => {
    const free = room.current;
    setItems((all) =>
      all.map((it) => {
        if (!pick(it)) return it;
        if (free !== null && it.file.size > free) {
          return { ...it, state: "failed", error: noRoom(it.file, free), retryable: true };
        }
        return { ...it, ...change, state: "queued", error: null };
      }),
    );
  }, []);

  const replace = useCallback((id: number) => requeue((it) => it.id === id, { overwrite: true }), [requeue]);
  const retry = useCallback((id: number) => requeue((it) => it.id === id, {}), [requeue]);
  /** remove cancels an upload in flight, or takes any other one off the list. */
  const remove = useCallback(
    (id: number) => {
      if (running.current === id) {
        cancelled.current = id;
        abort.current?.abort();
      } else {
        discard(id);
        drop(id);
      }
    },
    [drop, discard],
  );
  const replaceAll = useCallback(() => requeue((it) => it.state === "exists", { overwrite: true }), [requeue]);
  const skipAll = useCallback(() => {
    for (const it of current.current) if (it.state === "exists") discard(it.id);
    setItems((all) => all.filter((it) => it.state !== "exists"));
  }, [discard]);
  const clearDone = useCallback(() => setItems((all) => all.filter((it) => it.state !== "done")), []);

  const busy = items.some((it) => it.state === "queued" || it.state === "uploading");
  return { items, busy, add, replace, retry, remove, replaceAll, skipAll, clearDone };
}

function noRoom(file: File, free: number): string {
  return i18next.t("files:upload_no_room", { free: formatBytes(free), size: formatBytes(file.size) });
}
