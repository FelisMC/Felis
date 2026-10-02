// Runtime config: the panel is domain-agnostic at BUILD time (CI red line — no
// rootDomain literal may appear in source). apiBase + rootDomain are fetched from
// /config.json at boot, so one build serves any deployment (mirrors the Go side's
// --set root_domain portability, spec §16/§27). A dev fallback reads VITE_* env.

/** BuildInfo is the server's build stamp (internal/panel buildInfo). */
export interface BuildInfo {
  /** The raw stamp, e.g. "v1.0.0-earlyAccess+g1a2b3c4". */
  version: string;
  /** The tag without any commit suffix, e.g. "v1.0.0-earlyAccess". */
  release: string;
  /** Short commit of a dev build; absent for a clean release. */
  commit?: string;
  /** Anything but a clean release build. */
  dev: boolean;
}

export interface RuntimeConfig {
  distributed?: boolean;
  apiBase: string;
  rootDomain: string;
  /** Player-console hostname (console.<root>), absent when unconfigured. */
  panelHostname?: string;
  /** Operator-console hostname (op.console.<root>), absent when unconfigured. */
  adminHostname?: string;
  /** The public Minecraft port, absent when it is the default 25565. */
  gamePort?: number;
  /** What the server runs, for the version badge. */
  build?: BuildInfo;
  /** /config.json could not be read, so these are build-time defaults and
   *  addresses built from them may be wrong. The shell says so. */
  fallback?: boolean;
}

const FALLBACK: RuntimeConfig = {
  apiBase: import.meta.env.VITE_API_BASE ?? "/api/v1",
  rootDomain: import.meta.env.VITE_ROOT_DOMAIN ?? "localhost",
  fallback: true,
};

// A port that could not be dialed is dropped; the bare hostname is the safer guess.
function parsePort(raw: unknown): number | undefined {
  return Number.isInteger(raw) && (raw as number) > 0 && (raw as number) <= 65535
    ? (raw as number)
    : undefined;
}

function parseBuild(raw: unknown): BuildInfo | undefined {
  if (!raw || typeof raw !== "object") return undefined;
  const b = raw as Partial<BuildInfo>;
  if (typeof b.version !== "string" || typeof b.release !== "string") return undefined;
  return {
    version: b.version,
    release: b.release,
    commit: typeof b.commit === "string" && b.commit ? b.commit : undefined,
    dev: b.dev === true,
  };
}

/** versionLabel is what the badge shows: the release, plus the commit for a
 *  dev build (the Go side's documented rendering). */
export function versionLabel(b: BuildInfo): string {
  return b.dev && b.commit ? `${b.release}+${b.commit}` : b.release;
}

let cached: RuntimeConfig | null = null;

/** loadConfig fetches /config.json once and caches it; failures fall back to env
 *  so the SPA still mounts, marked `fallback` so the shell can say so. */
export async function loadConfig(): Promise<RuntimeConfig> {
  if (cached) return cached;
  try {
    const res = await fetch("/config.json", { cache: "no-store" });
    if (!res.ok) throw new Error(`config.json ${res.status}`);
    const raw = (await res.json()) as Partial<RuntimeConfig> | null;
    // Every address the panel shows is built on rootDomain; a file without one
    // is as good as none.
    if (typeof raw?.rootDomain !== "string" || raw.rootDomain === "") {
      throw new Error("config.json has no rootDomain");
    }
    cached = {
      apiBase: raw.apiBase ?? FALLBACK.apiBase,
      distributed: raw.distributed === true,
      rootDomain: raw.rootDomain,
      panelHostname: raw.panelHostname,
      adminHostname: raw.adminHostname,
      gamePort: parsePort(raw.gamePort),
      build: parseBuild(raw.build),
    };
  } catch (err) {
    console.warn("felis panel: /config.json unavailable, using build-time defaults", err);
    cached = FALLBACK;
  }
  return cached;
}

/** hostFor composes a server's public hostname from its subdomain and the runtime
 *  rootDomain — never a hardcoded domain (red line). */
export function hostFor(subdomain: string, cfg: RuntimeConfig): string {
  return `${subdomain}.${cfg.rootDomain}`;
}

/** joinAddress is what a player types into Minecraft to reach a server: its
 *  hostname, with the port only when the proxy listens off the default 25565. */
export function joinAddress(subdomain: string, cfg: RuntimeConfig): string {
  const host = hostFor(subdomain, cfg);
  return cfg.gamePort && cfg.gamePort !== 25565 ? `${host}:${cfg.gamePort}` : host;
}

// The IPv4 address a nip.io or sslip.io root domain spells out ("203.0.113.7.nip.io"),
// read as strictly as Go's net.ParseIP: four parts, each 0-255, no leading zeros.
function embeddedIPv4(rootDomain: string): string {
  const domain = rootDomain.trim().replace(/\.$/, "");
  for (const suffix of [".nip.io", ".sslip.io"]) {
    if (!domain.endsWith(suffix)) continue;
    const base = domain.slice(0, -suffix.length);
    const parts = base.split(".");
    if (parts.length === 4 && parts.every((p) => /^(0|[1-9]\d{0,2})$/.test(p) && Number(p) <= 255)) {
      return base;
    }
  }
  return "";
}

/** entryAddress is where anyone joins in Minecraft to reach the login server, which
 *  hands out link codes: the IP a nip.io or sslip.io root domain spells out, otherwise
 *  the root domain, with the port when it is not 25565. It mirrors the Go side's
 *  setupGameAddress, so the page and the `felis setup` terminal name the same address.
 *  Empty when config.json could not be read, as the root domain is then a guess. */
export function entryAddress(cfg: RuntimeConfig): string {
  if (cfg.fallback) return "";
  const host = embeddedIPv4(cfg.rootDomain) || cfg.rootDomain.trim().replace(/\.$/, "");
  if (!host) return "";
  return cfg.gamePort && cfg.gamePort !== 25565 ? `${host}:${cfg.gamePort}` : host;
}

/** isIPAddress reports whether an entry address names its host by IPv4 address. */
export function isIPAddress(address: string): boolean {
  return /^\d{1,3}(\.\d{1,3}){3}(:\d+)?$/.test(address);
}
