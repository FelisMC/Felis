import type {
  AccessResult,
  ApiError,
  AutostartPolicy,
  BackupView,
  BanlistResult,
  Build,
  CreateServerRequest,
  FleetServer,
  Identity,
  KickResult,
  LinkResult,
  LinkStatus,
  LoginResult,
  BindResult,
  PlayersResult,
  ServerInfo,
  WhitelistImage,
  WhitelistResult,
  Submission,
  UpdateWindow,
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

async function requestRaw<T>(
  method: string,
  path: string,
  body: Blob,
  headers?: Record<string, string>,
): Promise<T> {
  const { apiBase } = await loadConfig();
  const res = await fetch(`${apiBase}${path}`, {
    method,
    credentials: "include",
    headers,
    body,
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

  bind: (code: string) =>
    request<BindResult>("POST", "/auth/bind", { code }),

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

  // fleet is the SysAdmin cockpit's fleet-wide read (admin-tier GET /fleet): every
  // server's CRD lifecycle view plus its owner. It 403s for a non-admin principal —
  // the panel only renders the cockpit link behind is_admin, and the route guards
  // again server-side regardless of what the UI shows.
  fleet: () =>
    request<{ servers: FleetServer[] }>("GET", "/fleet").then((r) => r.servers ?? []),

  status: (name: string) => request<ServerInfo>("GET", `/servers/${name}/status`),

  wake: (name: string) =>
    request<{ name: string; desiredState: string }>("POST", `/servers/${name}/wake`),

  stop: (name: string) =>
    request<{ name: string; desiredState: string }>("POST", `/servers/${name}/stop`),

  claim: (name: string) =>
    request<{ name: string; claimed: boolean }>("POST", `/servers/${name}/claim`),

  /** sendCommand runs one RCON command against a running server (spec §8 写=RCON).
   *  The backend strips a leading "/", rejects control characters (newline → 400)
   *  and caps the command at 1000 bytes. The reply is the server's plain-text
   *  response body. */
  sendCommand: (name: string, command: string) =>
    request<{ output: string }>("POST", `/servers/${name}/command`, { command }),

  // Access control (spec §7 access). The backend translates these STRUCTURED fields
  // into RCON commands — every field is charset-validated server-side before it is
  // concatenated, so there is no free-text injection surface. All are owner-or-admin
  // gated and require the server to be Running (409 `not_running` otherwise), so the
  // panel only exposes them on a running server. The reply's `output` is the raw RCON
  // text, surfaced verbatim as confirmation.

  /** accessWhitelistList reads the server's whitelist. This GET ALSO requires a
   *  Running server (the readiness gate covers the read, not just the writes), so
   *  callers must gate the fetch on phase === "Running". */
  accessWhitelistList: (name: string) =>
    request<WhitelistResult>("GET", `/servers/${name}/access/whitelist`),

  accessWhitelist: (name: string, action: "add" | "remove", player: string) =>
    request<AccessResult>("POST", `/servers/${name}/access/whitelist`, {
      action,
      player,
    }),

  /** accessBanList reads the server's ban list. Like accessWhitelistList this GET
   *  requires a Running server (the readiness gate covers the read too), so callers
   *  gate the fetch on phase === "Running". */
  accessBanList: (name: string) =>
    request<BanlistResult>("GET", `/servers/${name}/access/ban`),

  accessBan: (name: string, action: "ban" | "pardon", player: string) =>
    request<AccessResult>("POST", `/servers/${name}/access/ban`, {
      action,
      player,
    }),

  /** accessPlayers reads WHO is online (the only source of names — status carries
   *  the count alone). Like accessWhitelistList this GET requires a Running server,
   *  so callers gate the fetch on phase === "Running". */
  accessPlayers: (name: string) =>
    request<PlayersResult>("GET", `/servers/${name}/access/players`),

  accessKick: (name: string, player: string) =>
    request<KickResult>("POST", `/servers/${name}/access/kick`, { player }),

  listImages: () =>
    request<{ images: WhitelistImage[] }>("GET", "/images").then((r) => r.images ?? []),

  addImage: (imageRef: string) =>
    request<WhitelistImage>("POST", "/images", { image_ref: imageRef }),

  removeImage: (imageRef: string) =>
    request<void>("DELETE", `/images?ref=${encodeURIComponent(imageRef)}`),

  buildImage: (req: { image_ref: string; dockerfile: string; context_ref: string; base_image?: string }) =>
    request<Build>("POST", "/images/build", req),

  getBuild: (id: string) =>
    request<Build>("GET", `/images/build/${id}`),

  cancelBuild: (id: string) =>
    request<Build>("POST", `/images/build/${id}/cancel`),

  createServer: (req: CreateServerRequest) =>
    request<{ name: string; subdomain: string; desiredState: string }>(
      "POST",
      "/servers",
      req,
    ),

  patchServer: (name: string, req: {
    displayName?: string;
    autostartPolicy?: AutostartPolicy;
    image?: string;
    memory?: string;
    resources?: {
      cpu?: string;
      cpuRequest?: string;
      memory?: string;
      memoryRequest?: string;
    };
  }) =>
    request<{ name: string; desiredState: string }>(
      "PATCH",
      `/servers/${name}`,
      req,
    ),

  // World backups (spec §7). listBackups is the app-tier read: an admin sees every
  // present backup, a user only the backups of worlds they formerly owned — the
  // scope is decided server-side from the principal, not by any client filter, so a
  // user cannot widen it. Only present (restorable) rows come back, newest first;
  // there is no per-server backups endpoint, so the panel filters by server_name
  // client-side and the first matching row is the one a restore would recover.
  listBackups: () =>
    request<{ backups: BackupView[] }>("GET", "/backups").then((r) => r.backups ?? []),

  // restoreBackup starts an ASYNC restore of a server's world from a backup
  // (spec §7 POST restore-backup). It accepts an optional backupId in the body: when
  // absent the backend restores the latest backup and resolves its opaque ref
  // server-side — the client never names a backup by handle (spec §286).
  // Preconditions are enforced server-side and surfaced as codes: owner-or-admin +
  // former-owner match (403), a present backup must exist (404 no_backup), and the
  // server MUST be fully stopped (409 not_stopped) since the restore writes into
  // the live world volume. The reply is 202 {name, status:"restoring", backup_id} —
  // success means the restore Job was enqueued, not that the world is back yet.
  restoreBackup: (name: string, backupId?: string) =>
    request<{ name: string; status: string; backup_id: string }>(
      "POST",
      `/servers/${name}/restore-backup`,
      backupId ? { backup_id: backupId } : undefined,
    ),

  // Account linking (spec §10). Both are POST: start reports status from the
  // session principal (no body, side-effect-free), verify consumes a code the
  // player was shown in-game. The panel can never mint a code — that is the
  // internal in-game face — so there is no client method for it.
  linkStatus: () => request<LinkStatus>("POST", "/account/link/start"),

  linkVerify: (code: string) =>
    request<LinkResult>("POST", "/account/link/verify", { code }),

  emailStart: (email: string) =>
    request<{ sent: boolean; expires_at: string }>("POST", "/account/email/start", { email }),

  emailVerify: (code: string) =>
    request<{ verified: boolean; email: string }>("POST", "/account/email/verify", { code }),

  passkeyRegisterBegin: () =>
    request<any>("POST", "/account/passkey/register/begin"),

  passkeyRegisterFinish: (name: string, attestation: any) =>
    request<any>("POST", "/account/passkey/register/finish", { name, attestation }),

  passkeyList: () =>
    request<{ credentials: any[] }>("GET", "/account/passkey/credentials"),

  passkeyDelete: (id: string) =>
    request<void>("DELETE", `/account/passkey/credentials/${id}`),

  listSubmissions: () =>
    request<{ submissions: Submission[] }>("GET", "/submissions").then((r) => r.submissions ?? []),

  approveSubmission: (id: string) =>
    request<Submission>("POST", `/submissions/${id}/approve`),

  rejectSubmission: (id: string, reason: string) =>
    request<Submission>("POST", `/submissions/${id}/reject`, { reason }),

  listMySubmissions: () =>
    request<{ submissions: Submission[] }>("GET", "/me/submissions").then((r) => r.submissions ?? []),

  createSubmission: (displayName: string) =>
    request<Submission>("POST", "/me/submissions", { display_name: displayName }),

  uploadSubmissionContext: (id: string, file: Blob) =>
    requestRaw<Submission>("POST", `/me/submissions/${id}/context`, file, {
      "Content-Type": "application/x-gzip",
    }),

  getUpdateWindow: () => request<UpdateWindow>("GET", "/updates/window"),

  setUpdateWindow: (window: UpdateWindow) => request<UpdateWindow>("PUT", "/updates/window", window),
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

/**
 * buildLogsStreamURL builds the SSE endpoint for build logs.
 */
export function buildLogsStreamURL(apiBase: string, id: string): string {
  return `${apiBase}/images/build/${encodeURIComponent(id)}/logs`;
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
    case "otp_resend_cooldown":
      return t("otp_resend_cooldown");
    case "otp_locked":
      return t("otp_locked");
    case "passkey_challenge_invalid":
      return t("passkey_challenge_invalid");
    case "invalid_attestation":
      return t("invalid_attestation");
    case "passkey_already_bound":
      return t("passkey_already_bound");
    case "passkey_unavailable":
      return t("passkey_unavailable");
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
    // Access control (spec §7): the server must be Running for any RCON-backed
    // access change; the panel gates on phase, but a stale phase can still race.
    case "not_running":
      return t("not_running");
    case "console_unavailable":
      return t("console_unavailable");
    // World restore (spec §7 restore-backup): the world volume must be free, so a
    // running/starting server 409s not_stopped; no present backup 404s no_backup;
    // the restore subsystem may be unwired (503 restore_unavailable).
    case "no_backup":
      return t("no_backup");
    case "not_stopped":
      return t("not_stopped");
    case "restore_unavailable":
      return t("restore_unavailable");
    default:
      if (err.status === 401) return t("session_expired");
      if (err.status === 403) return t("forbidden");
      return err.message ?? t("generic");
  }
}
