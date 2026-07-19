// Runtime config: the panel is domain-agnostic at BUILD time (CI red line — no
// rootDomain literal may appear in source). apiBase + rootDomain are fetched from
// /config.json at boot, so one build serves any deployment (mirrors the Go side's
// --set root_domain portability, spec §16/§27). A dev fallback reads VITE_* env.

export interface RuntimeConfig {
  apiBase: string;
  rootDomain: string;
  /** Player-console hostname (console.<root>), absent when unconfigured. */
  panelHostname?: string;
  /** Operator-console hostname (op.console.<root>), absent when unconfigured. */
  adminHostname?: string;
}

const FALLBACK: RuntimeConfig = {
  apiBase: import.meta.env.VITE_API_BASE ?? "/api/v1",
  rootDomain: import.meta.env.VITE_ROOT_DOMAIN ?? "localhost",
};

let cached: RuntimeConfig | null = null;

/** loadConfig fetches /config.json once and caches it; failures fall back to env
 *  so the SPA still mounts (and surfaces the misconfig in the UI). */
export async function loadConfig(): Promise<RuntimeConfig> {
  if (cached) return cached;
  try {
    const res = await fetch("/config.json", { cache: "no-store" });
    if (!res.ok) throw new Error(`config.json ${res.status}`);
    const raw = (await res.json()) as Partial<RuntimeConfig>;
    cached = {
      apiBase: raw.apiBase ?? FALLBACK.apiBase,
      rootDomain: raw.rootDomain ?? FALLBACK.rootDomain,
      panelHostname: raw.panelHostname,
      adminHostname: raw.adminHostname,
    };
  } catch {
    cached = FALLBACK;
  }
  return cached;
}

/** hostFor composes a server's public hostname from its subdomain and the runtime
 *  rootDomain — never a hardcoded domain (red line). */
export function hostFor(subdomain: string, cfg: RuntimeConfig): string {
  return `${subdomain}.${cfg.rootDomain}`;
}
