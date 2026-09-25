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
  apiBase: string;
  rootDomain: string;
  /** Player-console hostname (console.<root>), absent when unconfigured. */
  panelHostname?: string;
  /** Operator-console hostname (op.console.<root>), absent when unconfigured. */
  adminHostname?: string;
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
      rootDomain: raw.rootDomain,
      panelHostname: raw.panelHostname,
      adminHostname: raw.adminHostname,
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
