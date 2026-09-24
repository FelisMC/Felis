import { lazy, type ComponentType } from "react";

// A deploy replaces the hashed chunks, so a tab opened before it asks for files
// that no longer exist (panel.go answers index.html and the dynamic import
// fails). One full reload picks up the new index and its chunk names; the
// sessionStorage stamp keeps a real outage from turning into a reload loop.

const RELOAD_KEY = "felis:chunk-reload-at";
const RELOAD_WINDOW_MS = 30_000;

const CHUNK_ERROR =
  /Failed to fetch dynamically imported module|error loading dynamically imported module|Importing a module script failed|Unable to preload CSS|ChunkLoadError/i;

/** isChunkLoadError recognizes a failed lazy import in Chrome, Firefox and Safari. */
export function isChunkLoadError(err: unknown): boolean {
  const text = err instanceof Error ? `${err.name} ${err.message}` : String(err ?? "");
  return CHUNK_ERROR.test(text);
}

type Stamp = Pick<Storage, "getItem" | "setItem">;

/** claimChunkReload reports whether this tab may reload now and, if so, stamps
 *  the attempt. Without usable storage there is no loop guard, so it declines. */
export function claimChunkReload(storage: Stamp | null, now: number): boolean {
  if (!storage) return false;
  try {
    const last = Number(storage.getItem(RELOAD_KEY) ?? 0);
    if (Number.isFinite(last) && now - last < RELOAD_WINDOW_MS) return false;
    storage.setItem(RELOAD_KEY, String(now));
    return true;
  } catch {
    return false;
  }
}

/** reloadForNewDeploy reloads the page once per window; false means the caller
 *  must surface the error instead. */
export function reloadForNewDeploy(): boolean {
  let storage: Stamp | null = null;
  try {
    storage = window.sessionStorage;
  } catch {
    // Storage blocked (privacy mode): no guard, no automatic reload.
  }
  if (!claimChunkReload(storage, Date.now())) return false;
  window.location.reload();
  return true;
}

/** lazyWithReload is React.lazy that reloads once when the chunk has gone. */
export function lazyWithReload<T extends ComponentType<any>>(factory: () => Promise<{ default: T }>) {
  return lazy(() =>
    factory().catch((err: unknown) => {
      if (isChunkLoadError(err) && reloadForNewDeploy()) return new Promise<{ default: T }>(() => {});
      throw err;
    }),
  );
}

/** installPreloadReload covers Vite's own modulepreload/CSS preloads, which
 *  fail through the vite:preloadError event instead of the import promise. */
export function installPreloadReload(): void {
  window.addEventListener("vite:preloadError", (event) => {
    if (reloadForNewDeploy()) event.preventDefault();
  });
}
