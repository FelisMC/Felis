import { describe, it, expect } from "vitest";
import { claimChunkReload, isChunkLoadError } from "./chunk";

// After a deploy an old tab's lazy import 404s. The panel reloads once to pick up
// the new chunks; these cases pin which errors count and that a second failure
// inside the window surfaces instead of looping.

function memory(): Pick<Storage, "getItem" | "setItem"> & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return { data, getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
}

describe("isChunkLoadError", () => {
  it("recognizes each browser's wording", () => {
    for (const msg of [
      "Failed to fetch dynamically imported module: https://x/assets/VoxelFleet-abc.js",
      "error loading dynamically imported module",
      "Importing a module script failed.",
      "Unable to preload CSS for /assets/index-abc.css",
    ]) {
      expect(isChunkLoadError(new TypeError(msg))).toBe(true);
    }
  });

  it("leaves ordinary render errors alone", () => {
    expect(isChunkLoadError(new TypeError("Cannot read properties of undefined (reading 'name')"))).toBe(false);
    expect(isChunkLoadError(null)).toBe(false);
  });
});

describe("claimChunkReload", () => {
  it("allows the first reload and stamps it", () => {
    const s = memory();
    expect(claimChunkReload(s, 1_000_000)).toBe(true);
    expect(s.data.size).toBe(1);
  });

  it("refuses a second reload inside the window", () => {
    const s = memory();
    claimChunkReload(s, 1_000_000);
    expect(claimChunkReload(s, 1_010_000)).toBe(false);
  });

  it("allows another reload once the window has passed", () => {
    const s = memory();
    claimChunkReload(s, 1_000_000);
    expect(claimChunkReload(s, 1_031_000)).toBe(true);
  });

  it("declines without storage, since nothing could stop a loop", () => {
    expect(claimChunkReload(null, 1_000_000)).toBe(false);
    const broken = { getItem: () => { throw new Error("denied"); }, setItem: () => {} };
    expect(claimChunkReload(broken, 1_000_000)).toBe(false);
  });
});
