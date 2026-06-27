import type {
  ApiError,
  CreateServerRequest,
  Identity,
  LinkResult,
  LinkStatus,
  LoginResult,
  ServerInfo,
  WhitelistImage,
} from "./types";
import { loadConfig } from "./config";
import i18next from "i18next";

// Typed client for the felis-api external face (spec §7). Credentials are sent so
// the upstream Zero-Trust / Access cookie rides along; the panel never holds a
// service token, and the RCON password is never requested (spec §8).

function isApiError(x: unknown): x is { error: { code: string; message: string } } {
  return (
    typeof x === "object" &&
    x !== null &&
    "error" in x &&
    typeof (x as { error: unknown }).error === "object"
  );
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const { apiBase } = await loadConfig();
  const res = await fetch(`${apiBase}${path}`, {
    method,
    credentials: "include",
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });

  const text = await res.text();
  const parsed: unknown = text ? JSON.parse(text) : null;

  if (!res.ok) {
    const err: ApiError = {
      status: res.status,
      code: isApiError(parsed) ? parsed.error.code : "error",
      message: isApiError(parsed) ? parsed.error.message : res.statusText,
    };
    throw err;
  }
  return parsed as T;
}

export const api = {
  // Local-password auth (spec §B1). login sets an HttpOnly session cookie as a
  // side effect — the panel never sees it — and returns only what to route on next
  // (must_change_password forces the change card before any other surface). The
  // username/password pair is the ONLY local credential; Passkey/PWA are Phase
  // B2/C. login may 403 `local_auth_disabled` on a Zero-Trust-only deployment.
  login: (username: string, password: string) =>
    request<LoginResult>("POST", "/auth/login", { username, password }),

  // logout is idempotent server-side (clears the session row + cookie); calling it
  // without a session still resolves 200. After it, refreshing /me yields 401, which
  // the tier model reads as `unauthenticated` and routes back to /login.
  logout: () => request<{ ok: boolean }>("POST", "/auth/logout"),

  // changePassword is callable during the first-login lockdown (the route is
  // AllowDuringPasswordChange): the server re-verifies current_password, rejects an
  // unchanged or weak (8–72 byte) new password, writes the new hash, and revokes
  // every OTHER session. The caller's own session is kept, so no re-login is needed.
  changePassword: (current_password: string, new_password: string) =>
    request<{ ok: boolean }>("POST", "/auth/change-password", {
      current_password,
      new_password,
    }),

  // Identity (spec §7 GET /me) — the tier keystone. is_admin is server-computed
  // (Principal.IsAdmin); the panel reads it but re-deriving admin-ness is the
  // backend's job. Drives nav + route guards only; every admin route 403s on its
  // own regardless of what the panel renders.
  me: () => request<Identity>("GET", "/me"),

  myServers: () =>
    request<{ servers: ServerInfo[] }>("GET", "/me/servers").then((r) => r.servers ?? []),

  status: (name: string) => request<ServerInfo>("GET", `/servers/${name}/status`),

  wake: (name: string) =>
    request<{ name: string; desiredState: string }>("POST", `/servers/${name}/wake`),

  stop: (name: string) =>
    request<{ name: string; desiredState: string }>("POST", `/servers/${name}/stop`),

  claim: (name: string) =>
    request<{ name: string; claimed: boolean }>("POST", `/servers/${name}/claim`),

  listImages: () =>
    request<{ images: WhitelistImage[] }>("GET", "/images").then((r) => r.images ?? []),

  createServer: (req: CreateServerRequest) =>
    request<{ name: string; subdomain: string; desiredState: string }>(
      "POST",
      "/servers",
      req,
    ),

  // Account linking (spec §10). Both are POST: start reports status from the
  // session principal (no body, side-effect-free), verify consumes a code the
  // player was shown in-game. The panel can never mint a code — that is the
  // internal in-game face — so there is no client method for it.
  linkStatus: () => request<LinkStatus>("POST", "/account/link/start"),

  linkVerify: (code: string) =>
    request<LinkResult>("POST", "/account/link/verify", { code }),
};

/**
 * consoleStreamURL builds the §8 read-side SSE endpoint for a server. It mirrors
 * request()'s `${apiBase}${path}` join, but is a plain string builder rather than
 * a fetch: an EventSource is *constructed* from a URL (and carries the Access
 * cookie via withCredentials), it is not requested through this module. The name
 * is percent-encoded defensively — server names are validated `[a-z0-9-]`
 * upstream, but the URL is built from a router param, so encoding keeps a stray
 * value from breaking the URL.
 */
export function consoleStreamURL(apiBase: string, name: string): string {
  return `${apiBase}/servers/${encodeURIComponent(name)}/console`;
}

/** humanizeError turns the stable error code into a user-facing line. */
export function humanizeError(e: unknown): string {
  const err = e as Partial<ApiError>;
  const t = i18next.getFixedT(null, "errors");
  switch (err.code) {
    // Local-password auth (spec §B1).
    case "local_auth_disabled":
      return t("local_auth_disabled");
    case "invalid_credentials":
      return t("invalid_credentials");
    case "weak_password":
      return t("weak_password");
    case "password_unchanged":
      return t("password_unchanged");
    case "not_linked":
      return t("not_linked");
    case "invalid_code":
      return t("invalid_code");
    case "already_linked":
      return t("already_linked");
    case "quota_exceeded":
      return t("quota_exceeded");
    case "already_claimed":
      return t("already_claimed");
    case "image_not_whitelisted":
      return t("image_not_whitelisted");
    case "subdomain_taken":
      return t("subdomain_taken");
    case "already_exists":
      return t("already_exists");
    case "cooldown":
      return t("cooldown");
    default:
      if (err.status === 401) return t("session_expired");
      if (err.status === 403) return t("forbidden");
      return err.message ?? t("generic");
  }
}
