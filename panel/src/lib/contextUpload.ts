import { api } from "./api";
import type { ApiError } from "./types";

// Split contexts below proxy body limits and resume from the server's staged offset after transient failures.

export interface UploadContextOptions {
  /** Reports the bytes of file sent so far (stored plus the part in flight)
   *  and, as stored, the bytes the server has confirmed holding. */
  onProgress?: (sent: number, total: number, stored: number) => void;
  /** Cancels the upload; the staged parts stay for a later resume. */
  signal?: AbortSignal;
  // Resume only when staged parts match this file's prefix; otherwise discard the earlier upload.
  resume?: boolean;
  /** Test seam for the pause between attempts. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

/** Consecutive failed attempts a part may take before the upload gives up. */
export const MAX_ATTEMPTS = 8;
/** Checks while the server finishes storing a large context (about 10 minutes). */
export const MAX_COMPLETE_WAITS = 40;

// Retry transport, upstream, storage, digest, and recoverable upload-state errors.
export function isTransient(e: unknown): boolean {
  const err = e as Partial<ApiError> | null;
  if (!err || typeof err.code !== "string") return false;
  return (
    err.code === "network_error" ||
    err.code === "upstream_unavailable" ||
    err.code === "uploads_store_unavailable" ||
    err.code === "upload_busy" ||
    err.code === "digest_mismatch" ||
    err.code === "upload_offset_mismatch"
  );
}

/** retryDelay is the pause before attempt n+1: 1s, 2s, 4s, 8s, then 15s. */
export function retryDelay(n: number): number {
  return Math.min(1000 * 2 ** (n - 1), 15000);
}

/** wait pauses ms, or rejects with an AbortError as soon as signal aborts. */
export function wait(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException("The upload was cancelled", "AbortError"));
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(new DOMException("The upload was cancelled", "AbortError"));
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

export async function uploadContext(id: string, file: Blob, opts: UploadContextOptions = {}): Promise<void> {
  const { onProgress, signal } = opts;
  const sleep = opts.sleep ?? wait;
  const total = file.size;

  // offset is where the next part starts; null means "ask the server first".
  let offset: number | null = null;
  let partMax = 0;
  let fresh = !opts.resume;
  let failures = 0;
  for (;;) {
    try {
      if (offset === null) {
        const at = await api.getContextUpload(id);
        partMax = at.part_max_bytes;
        // More staged than this file holds cannot be a prefix of it.
        offset = fresh || at.received > total ? 0 : at.received;
        onProgress?.(offset, total, offset);
      }
      if (offset < total || offset === 0) {
        const start = offset;
        const part = file.slice(start, Math.min(start + partMax, total));
        const at = await api.putContextPart(id, start, part, {
          signal,
          onProgress: (sent) => onProgress?.(start + sent, total, start),
        });
        offset = at.received;
        fresh = false;
        failures = 0;
        onProgress?.(offset, total, offset);
      }
      if (offset >= total) break;
    } catch (e) {
      // A pause (AbortError) is not transient, so it ends the upload here.
      if (!isTransient(e) || ++failures >= MAX_ATTEMPTS) throw e;
      await sleep(retryDelay(failures), signal);
      offset = null;
    }
  }
  await complete(id, total, sleep, signal);
}

// Completion may outlive the edge timeout; recheck staging, where absence means the context was stored.
async function complete(
  id: string,
  total: number,
  sleep: (ms: number, signal?: AbortSignal) => Promise<void>,
  signal?: AbortSignal,
): Promise<void> {
  let lastErr: unknown;
  for (let waits = 0; waits <= MAX_COMPLETE_WAITS; waits++) {
    if (waits > 0) {
      await sleep(retryDelay(waits), signal);
      try {
        const at = await api.getContextUpload(id);
        if (at.received < total) {
          if (at.received === 0) return;
          throw lastErr;
        }
      } catch (e) {
        if (e === lastErr || !isTransient(e)) throw e;
        continue;
      }
    }
    try {
      await api.completeContextUpload(id);
      return;
    } catch (e) {
      if (!isTransient(e)) throw e;
      lastErr = e;
    }
  }
  throw lastErr;
}
