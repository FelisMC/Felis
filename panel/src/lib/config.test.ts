import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

// config.ts caches the first load, so every test imports a fresh copy.
async function freshConfig() {
  vi.resetModules();
  return import("./config");
}

function serve(body: unknown, status = 200) {
  const fetchMock = vi.fn(async () =>
    new Response(typeof body === "string" ? body : JSON.stringify(body), { status }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

const build = { version: "v1.2.0+g1a2b3c4", release: "v1.2.0", commit: "1a2b3c4", dev: true };

beforeEach(() => {
  vi.spyOn(console, "warn").mockImplementation(() => {});
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("loadConfig", () => {
  it("reads the server's config and build stamp", async () => {
    serve({ apiBase: "/api/v1", rootDomain: "mc.example", adminHostname: "op.console.mc.example", build });
    const { loadConfig } = await freshConfig();

    const cfg = await loadConfig();
    expect(cfg).toEqual({
      apiBase: "/api/v1",
      rootDomain: "mc.example",
      panelHostname: undefined,
      adminHostname: "op.console.mc.example",
      build,
    });
    expect(cfg.fallback).toBeUndefined();
  });

  it("fetches once and keeps the result", async () => {
    const fetchMock = serve({ apiBase: "/api/v1", rootDomain: "mc.example" });
    const { loadConfig } = await freshConfig();
    await loadConfig();
    await loadConfig();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("drops a build stamp it cannot read, keeping the rest", async () => {
    serve({ apiBase: "/api/v1", rootDomain: "mc.example", build: { version: 3 } });
    const { loadConfig } = await freshConfig();
    const cfg = await loadConfig();
    expect(cfg.build).toBeUndefined();
    expect(cfg.fallback).toBeUndefined();
  });

  it("reads a release build without a commit", async () => {
    serve({ rootDomain: "mc.example", build: { version: "v1.2.0", release: "v1.2.0", commit: "", dev: false } });
    const { loadConfig } = await freshConfig();
    expect((await loadConfig()).build).toEqual({ version: "v1.2.0", release: "v1.2.0", commit: undefined, dev: false });
  });

  const failures: Array<[string, () => void]> = [
    ["an error status", () => serve({ rootDomain: "mc.example" }, 502)],
    ["a network failure", () => vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new TypeError("offline"))))],
    ["a body that is not JSON", () => serve("<html>Access login</html>")],
    ["JSON null", () => serve("null")],
    ["no rootDomain", () => serve({ apiBase: "/api/v1" })],
    ["an empty rootDomain", () => serve({ apiBase: "/api/v1", rootDomain: "" })],
  ];
  for (const [what, arrange] of failures) {
    it(`marks the defaults as a fallback after ${what}`, async () => {
      arrange();
      const { loadConfig } = await freshConfig();
      const cfg = await loadConfig();
      expect(cfg.fallback).toBe(true);
      expect(cfg.apiBase).toBe("/api/v1");
      expect(cfg.rootDomain).toBe("localhost");
      expect(console.warn).toHaveBeenCalled();
    });
  }
});

describe("versionLabel", () => {
  it("shows a clean release as its tag", async () => {
    const { versionLabel } = await freshConfig();
    expect(versionLabel({ version: "v1.2.0", release: "v1.2.0", dev: false })).toBe("v1.2.0");
  });

  it("appends a commit only for a dev build, as the server documents", async () => {
    const { versionLabel } = await freshConfig();
    expect(versionLabel({ version: "v1.2.0", release: "v1.2.0", commit: "1a2b3c4", dev: false })).toBe("v1.2.0");
  });

  it("adds the commit to a dev build", async () => {
    const { versionLabel } = await freshConfig();
    expect(versionLabel(build)).toBe("v1.2.0+1a2b3c4");
  });

  it("shows a dev build without a commit as its release", async () => {
    const { versionLabel } = await freshConfig();
    expect(versionLabel({ version: "dev", release: "dev", dev: true })).toBe("dev");
  });
});

describe("joinAddress", () => {
  const base = { apiBase: "/api/v1", rootDomain: "mc.example" };

  it("is the bare hostname on the default port", async () => {
    const { joinAddress } = await freshConfig();
    expect(joinAddress("survival", base)).toBe("survival.mc.example");
    expect(joinAddress("survival", { ...base, gamePort: 25565 })).toBe("survival.mc.example");
  });

  it("adds a port the proxy moved off the default", async () => {
    const { joinAddress } = await freshConfig();
    expect(joinAddress("survival", { ...base, gamePort: 25570 })).toBe("survival.mc.example:25570");
  });

  it("reads the port from config.json and drops one no client could dial", async () => {
    serve({ apiBase: "/api/v1", rootDomain: "mc.example", gamePort: 25570 });
    let mod = await freshConfig();
    expect((await mod.loadConfig()).gamePort).toBe(25570);

    for (const bad of [0, 70000, "25570", 1.5]) {
      serve({ apiBase: "/api/v1", rootDomain: "mc.example", gamePort: bad });
      mod = await freshConfig();
      expect((await mod.loadConfig()).gamePort).toBeUndefined();
    }
  });
});
