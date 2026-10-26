import { api, clientError } from "@/lib/api";
import { sha256Of } from "@/lib/digest";
import { MAX_ATTEMPTS, isTransient, retryDelay, wait } from "@/lib/contextUpload";
import type { FileOp, FileUploadSession } from "@/lib/types";

// Remember per-path upload sessions; resume matching files from digest-verified staged offsets.

type Sleep = (ms: number, signal?: AbortSignal) => Promise<void>;

export const OP_POLL_MS = 2000;
export const MAX_OP_MISSES = 5; // missed reads of the ops list before an op counts as lost
export const STALE_SESSION_MS = 5 * 60 * 1000; // idle time before a remembered session may be given back; younger ones may still be sending

const KEY_PREFIX = "felis-file-upload:";

interface Remembered {
  server: string;
  path: string;
  id: string;
  size: number;
  modified: number;
  touched: number;
}

const code = (e: unknown) => (e as { code?: unknown } | null)?.code;

// Storage can be missing or refuse (private window).
function storage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

const keyOf = (server: string, path: string) => `${KEY_PREFIX}${server}:${path}`;

function remember(r: Remembered) {
  try {
    storage()?.setItem(keyOf(r.server, r.path), JSON.stringify(r));
  } catch {
    /* not remembered */
  }
}

function recalled(server: string, path: string, file: File): Remembered | null {
  try {
    const raw = storage()?.getItem(keyOf(server, path));
    const r = raw ? (JSON.parse(raw) as Remembered) : null;
    return r && r.id && r.size === file.size && r.modified === file.lastModified ? r : null;
  } catch {
    return null;
  }
}

function rememberedAll(): Remembered[] {
  const s = storage();
  const out: Remembered[] = [];
  try {
    for (let i = 0; s && i < s.length; i++) {
      const k = s.key(i);
      if (!k?.startsWith(KEY_PREFIX)) continue;
      const r = JSON.parse(s.getItem(k) ?? "null") as Remembered | null;
      if (r?.id && r.server && typeof r.path === "string") out.push(r);
    }
  } catch {
    /* keep what was read */
  }
  return out;
}

export function forgetSession(server: string, path: string) {
  try {
    storage()?.removeItem(keyOf(server, path));
  } catch {
    /* nothing to forget */
  }
}

/** Cancels and forgets a session; a busy part is waited out, and an undeletable session is dropped by the server after 6 idle hours. */
export async function discardSession(server: string, path: string, id: string, sleep: Sleep = wait) {
  forgetSession(server, path);
  for (let n = 1; n <= 3; n++) {
    try {
      await api.deleteServerFileUpload(server, id);
      return;
    } catch (e) {
      if (code(e) !== "upload_busy") return;
      await sleep(retryDelay(n));
    }
  }
}

// Cancels remembered sessions that have not sent a part lately; reports whether any was still held.
async function dropStale(now: number): Promise<boolean> {
  let freed = false;
  for (const r of rememberedAll()) {
    if (now - r.touched < STALE_SESSION_MS) continue;
    forgetSession(r.server, r.path);
    try {
      await api.deleteServerFileUpload(r.server, r.id);
      freed = true;
    } catch {
      /* gone already */
    }
  }
  return freed;
}

export interface SendOptions {
  overwrite: boolean;
  /** Bytes the server holds plus the part in flight. */
  onProgress?: (sent: number) => void;
  onSession?: (id: string) => void;
  beforeCommit?: () => void;
  /** Stops sending; the session stays remembered for a later resume. */
  signal?: AbortSignal;
  /** Test seam. */
  sleep?: Sleep;
  /** Test seam: the clock remembered sessions are aged by. */
  now?: () => number;
}

export async function sendInParts(server: string, path: string, file: File, opts: SendOptions): Promise<FileOp> {
  const { signal, onProgress } = opts;
  const sleep = opts.sleep ?? wait;
  const now = opts.now ?? Date.now;
  const size = file.size;

  let id: string | null = recalled(server, path, file)?.id ?? null;
  // null means "ask the server first"
  let offset: number | null = null;
  let partMax = 0;
  let failures = 0;
  // Bytes at the head of the session known to be the file's own (sent, or held parts hashed again).
  let known = 0;
  for (;;) {
    try {
      if (offset === null) {
        const at = await standing(server, path, file, id, now);
        id = at.id;
        opts.onSession?.(id);
        if (!(await holdsTheFile(file, at, known, onProgress, signal))) {
          await discardSession(server, path, at.id, sleep);
          id = null;
          continue;
        }
        known = at.received;
        partMax = at.part_max_bytes;
        offset = at.received;
        onProgress?.(offset);
      }
      if (offset >= size) break;
      const start = offset;
      const part = file.slice(start, Math.min(start + partMax, size));
      const at = await api.putServerFileUploadPart(server, id!, start, part, {
        signal,
        onProgress: (sent) => onProgress?.(start + sent),
      });
      offset = at.received;
      known = at.received;
      failures = 0;
      remember({ server, path, id: id!, size, modified: file.lastModified, touched: now() });
      onProgress?.(offset);
    } catch (e) {
      // upload_not_found (server restarted or session idle): begin a new session on the next pass.
      if (code(e) === "upload_not_found") {
        id = null;
      } else if (!isTransient(e)) {
        throw e;
      }
      if (++failures >= MAX_ATTEMPTS) throw e;
      await sleep(retryDelay(failures), signal);
      offset = null;
    }
  }
  opts.beforeCommit?.();
  return commit(server, path, id!, opts.overwrite, sleep, signal);
}

// Four sessions per account is the server's bound; on too_many_uploads this browser's stale sessions are given back first.
async function standing(
  server: string,
  path: string,
  file: File,
  id: string | null,
  now: () => number,
): Promise<FileUploadSession> {
  if (id) {
    try {
      const at = await api.getServerFileUpload(server, id);
      if (at.path === path && at.size === file.size) return at;
    } catch (e) {
      if (code(e) !== "upload_not_found") throw e;
    }
  }
  let at: FileUploadSession;
  try {
    at = await api.beginServerFileUpload(server, path, file.size);
  } catch (e) {
    if (code(e) !== "too_many_uploads" || !(await dropStale(now()))) throw e;
    at = await api.beginServerFileUpload(server, path, file.size);
  }
  remember({ server, path, id: at.id, size: file.size, modified: file.lastModified, touched: now() });
  return at;
}

// Hashes the session's held parts past `known` against the same ranges of file; true when all match.
async function holdsTheFile(
  file: File,
  at: FileUploadSession,
  known: number,
  onProgress?: (sent: number) => void,
  signal?: AbortSignal,
): Promise<boolean> {
  let start = 0;
  for (const part of at.parts) {
    const end = start + part.size;
    if (end > known) {
      if (signal?.aborted) throw new DOMException("The upload was cancelled", "AbortError");
      if ((await sha256Of(file.slice(start, end))).hex !== part.sha256) return false;
      onProgress?.(end);
    }
    start = end;
  }
  return start === at.received;
}

// After a lost commit reply, a busy or landed session reveals the created op.
async function commit(
  server: string,
  path: string,
  id: string,
  overwrite: boolean,
  sleep: Sleep,
  signal?: AbortSignal,
): Promise<FileOp> {
  let lost = false;
  for (let failures = 1; ; failures++) {
    try {
      return (await api.commitServerFileUpload(server, id, overwrite)).op;
    } catch (e) {
      const c = code(e);
      if (lost && (c === "maintenance_in_progress" || c === "upload_busy" || c === "upload_not_found")) {
        const { ops } = await api.listServerFileOps(server);
        const op = ops.find(
          (o) => o.op === "upload" && o.path === path && (c === "upload_not_found" || o.state === "running"),
        );
        if (op) return op;
        if (c === "upload_not_found") throw e;
      } else if (isTransient(e)) {
        lost = true;
      } else {
        throw e;
      }
      if (failures >= MAX_ATTEMPTS) throw e;
      await sleep(retryDelay(failures), signal);
    }
  }
}

export interface WatchOptions {
  signal?: AbortSignal;
  sleep?: Sleep;
  onProgress?: (op: FileOp) => void;
}

/** An op missing from MAX_OP_MISSES reads in a row is reported as op_lost. */
export async function watchOp(server: string, id: string, opts: WatchOptions = {}): Promise<FileOp> {
  const sleep = opts.sleep ?? wait;
  let failures = 0;
  let misses = 0;
  for (;;) {
    await sleep(failures > 0 ? retryDelay(failures) : OP_POLL_MS, opts.signal);
    let ops: FileOp[];
    try {
      ops = (await api.listServerFileOps(server)).ops;
      failures = 0;
    } catch (e) {
      if (!isTransient(e) || ++failures >= MAX_ATTEMPTS) throw e;
      continue;
    }
    const op = ops.find((o) => o.id === id);
    if (!op) {
      if (++misses >= MAX_OP_MISSES) throw clientError("op_lost");
      continue;
    }
    misses = 0;
    if (op.state !== "running") return op;
    opts.onProgress?.(op);
  }
}
