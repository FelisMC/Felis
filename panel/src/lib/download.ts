import { api } from "./api";
import type { ExportStatus } from "./types";

/** How often a ticket is read while its export Job gets the archive ready. */
export const EXPORT_POLL_MS = 2000;

const pause = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/** awaitExport reads a download's ticket until felis-api holds the archive
 *  ("ready") or its Job gave up ("failed"), and returns that answer; null once
 *  `alive` says the page asking has gone (the export is then abandoned: nobody
 *  fetches it). A ready ticket waits 90 s for the browser, so the caller opens
 *  it at once. A pending ticket ends on its own (410 export_expired after 10
 *  min), which bounds the wait. */
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

/** saveDownload hands a ready export to the browser the way a link would: a
 *  hidden <a download> it clicks once. felis-api answers with an attachment, so
 *  the browser's own download manager streams the archive to disk; reading it
 *  into a Blob first would hold a whole world in the tab's memory. */
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
