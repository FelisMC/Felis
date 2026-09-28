import { api, clientError } from "@/lib/api";
import { sha256Of } from "@/lib/digest";
import { MAX_ATTEMPTS, isTransient, retryDelay, wait } from "@/lib/contextUpload";
import type { FileOp, FileUploadSession } from "@/lib/types";

// A file bigger than one request carries goes up as a session (api
// beginServerFileUpload): parts of at most part_max_bytes, each at the byte
// offset the session has reached, so a dropped connection resumes where the
// server says it stands rather than starting over. The commit answers at once
// with the op landing the file, and watchOp follows that op to its end.
//
// The session is also remembered in this browser, under the server and path it
// lands at, with the file's size and modification time. Choosing the same file
// again after a reload, a closed tab or a lost connection carries on from the
// bytes already sent, once those have been hashed again against the file: the
// session lists each part it holds with its SHA-256, and a session holding
// anything else (another file of the same name, size and time) is given back
// and the file starts over. A remembered session no page has touched for a
// while is what a refusal for too many sessions gives back first.
//
// Every part goes with its SHA-256, and one the server hashes differently
// (digest_mismatch: changed on the way) is sent again.

type Sleep = (ms: number, signal?: AbortSignal) => Promise<void>;

/** How often a running op is read. */
export const OP_POLL_MS = 2000;
/** Reads of the ops list that may miss an op before it counts as lost. */
export const MAX_OP_MISSES = 5;
/** How long a remembered session must sit untouched before it is given back
 *  to make room for a new one; one another tab is still sending is younger. */
export const STALE_SESSION_MS = 5 * 60 * 1000;

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

// Browser storage can be missing or refuse (a private window, blocked site
// data); a session is then simply not remembered.
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
    /* not remembered: a reload starts this file over */
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
    /* whatever was read so far */
  }
  return out;
}

/** forgetSession stops remembering the session for path on server, once its
 *  file has landed. */
export function forgetSession(server: string, path: string) {
  try {
    storage()?.removeItem(keyOf(server, path));
  } catch {
    /* nothing to forget */
  }
}

/** discardSession cancels a session and forgets it. A part still arriving holds
 *  it briefly (upload_busy), so that is waited out a few times; whatever still
 *  fails is left for the server to drop after 6 idle hours. */
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

// dropStale cancels the remembered sessions nothing has sent a part to lately
// and reports whether any of them was still held.
async function dropStale(now: number): Promise<boolean> {
  let freed = false;
  for (const r of rememberedAll()) {
    if (now - r.touched < STALE_SESSION_MS) continue;
    forgetSession(r.server, r.path);
    try {
      await api.deleteServerFileUpload(r.server, r.id);
      freed = true;
    } catch {
      /* gone already, or someone else's */
    }
  }
  return freed;
}

export interface SendOptions {
  overwrite: boolean;
  /** Bytes of the file the server holds so far, plus the part in flight. */
  onProgress?: (sent: number) => void;
  /** The session the file goes up in, once there is one. */
  onSession?: (id: string) => void;
  /** Stops sending; the session stays, remembered, for a later resume. */
  signal?: AbortSignal;
  /** Test seam for the pause between attempts. */
  sleep?: Sleep;
  /** Test seam for the clock the remembered sessions are aged by. */
  now?: () => number;
}

/** sendInParts sends file to path on server as an upload session, commits it,
 *  and answers the op landing it. */
export async function sendInParts(server: string, path: string, file: File, opts: SendOptions): Promise<FileOp> {
  const { signal, onProgress } = opts;
  const sleep = opts.sleep ?? wait;
  const now = opts.now ?? Date.now;
  const size = file.size;

  let id: string | null = recalled(server, path, file)?.id ?? null;
  // offset is where the next part starts; null means "ask the server first".
  let offset: number | null = null;
  let partMax = 0;
  let failures = 0;
  // known is how many bytes at the head of the session this run has seen to be
  // the file's own: parts it sent, or held parts it hashed again. A session
  // begun during the run holds only parts the run sent, so a new one needs no
  // reset.
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
      // The session went away under the upload (felis-api restarted, or it sat
      // idle too long): the next pass begins a new one. A part changed on the
      // way was not kept, so the next pass sends it again (isTransient).
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
  return commit(server, path, id!, opts.overwrite, sleep, signal);
}

// standing answers the session to carry on with: id's when it still stands for
// this file, else a new one. Four sessions per account is the server's bound;
// when that refuses, the sessions this browser left behind are given back
// first.
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

// holdsTheFile hashes the parts session at holds past the first known bytes
// against the same ranges of file, and answers whether all of them match.
// onProgress walks up through the parts as they check out.
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

// commit lands the session. A commit whose answer was lost may still have
// started the Job, so asking again is read in that light: the world held
// (maintenance_in_progress) by an upload of this path running now, or the
// session gone because that Job reported the file landed, means the first
// commit went through, and its op is the answer.
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
      if (lost && (c === "maintenance_in_progress" || c === "upload_not_found")) {
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
  /** Each read of the op while it runs. */
  onProgress?: (op: FileOp) => void;
}

/** watchOp reads server's ops until the op id has ended, and answers it. A read
 *  that did not get through is tried again, backing off; an op missing from
 *  MAX_OP_MISSES reads in a row is reported as op_lost. */
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
