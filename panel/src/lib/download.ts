import { api } from "./api";
import type { ExportStatus } from "./types";

/** How often a ticket is read while its export Job gets the archive ready. */
export const EXPORT_POLL_MS = 2000;

const pause = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

// Stop polling after unmount; open ready tickets immediately within their 90-second lifetime.
// A pending ticket ends on its own (410 export_expired after 10 min), which bounds the wait.
export async function awaitExport(
  ticket: string,
  alive: () => boolean,
  sleep: (ms: number) => Promise<void> = pause,
): Promise<ExportStatus | null> {
  for (;;) {
    await sleep(EXPORT_POLL_MS);
    if (!alive()) return null;
    const s = await api.exportStatus(ticket);
    if (s.state !== "pending") return s;
  }
}

// Use an attachment link so the browser streams worlds without buffering them in a Blob.
/** saveBlob hands a file read into memory to the browser's download manager. */
export function saveBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  saveDownload(url, filename);
  window.setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

export function saveDownload(url: string, filename: string) {
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.rel = "noopener";
  a.hidden = true;
  document.body.appendChild(a);
  a.click();
  a.remove();
}
