import type {
  AccessResult,
  ApiError,
  AutostartPolicy,
  BackupView,
  BanlistResult,
  Build,
  CreateServerRequest,
  CreateUserRequest,
  FleetServer,
  Identity,
  KickResult,
  LinkResult,
  LinkStatus,
  BindResult,
  PatchUserRequest,
  PlayersResult,
  QuotaInput,
  QuotaView,
  ServerFileEntry,
  ServerJob,
  ServerInfo,
  SessionView,
  UserDetail,
  UserView,
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

// A locked session — one that still owes the forced onboarding (passkey
// enrollment) — gets `403 setup_required` from every protected route. Rendered as
// a generic permission error that reads as "you may not do this", when the truth
// is "one step remains and completing it unlocks the app" (#8). api.ts cannot
// navigate (no router here), so it announces the code on a window event; the
// App-shell listener routes the person to /setup, which resumes from the session
// without needing a token. Non-browser callers keep the plain error.
export const SETUP_REQUIRED_EVENT = "felis:setup-required";

function announceSetupRequired(err: ApiError): void {
  if (err.status !== 403 || err.code !== "setup_required") return;
  if (typeof window === "undefined") return;
  window.dispatchEvent(new Event(SETUP_REQUIRED_EVENT));
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
    announceSetupRequired(err);
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
    announceSetupRequired(err);
    throw err;
  }
  return parsed as T;
}

// Setup bootstrap (spec §B). The one-time token from `felis setup` is redeemed for
// a lockdown session; the response (and /setup/status) reports which onboarding
// steps remain so the Setup wizard can drive email verification + passkey enrollment.
export interface SetupState {
  user_id: string;
  username: string;
  role: string;
  email: string | null;
  email_verified: boolean;
  has_passkey: boolean;
  setup_required: boolean;
}

export const api = {
  // Session doors (spec §B). The product is passwordless: a session is minted only
  // by passkey, email-OTP, bind code, or the op-login vouch flow below. Every door
  // sets an HttpOnly cookie as a side effect and may 403 `local_auth_disabled` on a
  // Zero-Trust-only deployment.

  // logout is idempotent server-side (clears the session row + cookie); calling it
  // without a session still resolves 200. After it, refreshing /me yields 401, which
  // the tier model reads as `unauthenticated` and routes back to /login.
  logout: () => request<{ ok: boolean }>("POST", "/auth/logout"),

  bind: (code: string) =>
    request<BindResult>("POST", "/auth/bind", { code }),

  authEmailStart: (email: string) =>
    request<{ sent: boolean; expires_at: string }>("POST", "/auth/email/start", { email }),

  authEmailVerify: (email: string, code: string) =>
    request<{ user_id: string; role: string }>("POST", "/auth/email/verify", { email, code }),

  authPasskeyLoginBegin: (email: string) =>
    request<any>("POST", "/auth/passkey/login/begin", { email }),

  authPasskeyLoginFinish: (email: string, assertion: any) =>
    request<any>("POST", "/auth/passkey/login/finish", { email, assertion }),

  authPasskeyDiscoverableBegin: () =>
    request<any>("POST", "/auth/passkey/login/discoverable/begin", {}),

  authPasskeyDiscoverableFinish: (login_id: string, assertion: any) =>
    request<any>("POST", "/auth/passkey/login/discoverable/finish", { login_id, assertion }),

  // Op-login (spec §B): the staff door. start mails an OTP to a staff address and
  // returns a request handle; an online admin vouches in-game with
  // `/felis web op approve <request_id>`; the panel polls status until approved,
  // then finish redeems {request_id, code} into a session. start answers 202 with a
  // request_id for ANY well-formed address (anti-enumeration), so the UI just waits.
  opLoginStart: (email: string) =>
    request<{ request_id: string; expires_at: string }>("POST", "/auth/op-login/start", { email }),

  opLoginStatus: (id: string) =>
    request<{ approved: boolean }>("GET", `/auth/op-login/status/${encodeURIComponent(id)}`),

  opLoginFinish: (request_id: string, code: string) =>
    request<{ user_id: string; role: string }>("POST", "/auth/op-login/finish", {
      request_id,
      code,
    }),

  // Setup bootstrap (spec §B). redeem consumes the one-time token from the setup URL
  // and mints a lockdown session (Public); status re-reads progress for a reload
  // mid-wizard (SetupAllowed — the surviving session, no token needed).
  setupRedeem: (token: string) =>
    request<SetupState>("POST", "/auth/setup/redeem", { token }),

  setupStatus: () =>
    request<SetupState>("GET", "/auth/setup/status"),

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

  accessLuckPermsInfo: (name: string, player: string) =>
    request<{
      player: string;
      groups: string[];
      permissions: { node: string; value: boolean; world?: string }[];
      output: string;
    }>("GET", `/servers/${name}/access/luckperms/${player}`),

  accessPermission: (
    name: string,
    action: "set" | "unset",
    player: string,
    node: string,
    value?: boolean,
    world?: string
  ) =>
    request<AccessResult & { node: string; value?: boolean; world?: string }>(
      "POST",
      `/servers/${name}/access/permission`,
      { action, player, node, value, world }
    ),

  accessGroup: (
    name: string,
    action: "add" | "remove",
    player: string,
    group: string
  ) =>
    request<AccessResult & { group: string }>(
      "POST",
      `/servers/${name}/access/group`,
      { action, player, group }
    ),

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

  // backupNow enqueues a manual backup (spec §7 POST backup). Preconditions are
  // enforced server-side and surfaced as codes: owner-or-admin (403) and the
  // server MUST be fully stopped (409 not_stopped — the world volume is RWO), so
  // callers gate the action on phase === "Stopped". The reply is 202
  // {name, status:"backing_up"}: the Job is enqueued, not done — watch
  // serverJobs for the outcome.
  backupNow: (name: string) =>
    request<{ name: string; status: string }>("POST", `/servers/${name}/backup`),

  // serverJobs lists the newest backup/restore Jobs of one server, newest first
  // (GET /servers/{name}/jobs). Owner-or-admin gated server-side; a Job's
  // failure text rides `message`. The backend answers 503 until the job-status
  // reader is wired, so callers should tolerate that error.
  serverJobs: (name: string) =>
    request<{ server: string; jobs: ServerJob[] }>(
      "GET",
      `/servers/${name}/jobs`,
    ).then((r) => r.jobs ?? []),

  // Server file editor (spec §7). All three routes are owner-or-admin gated and
  // refuse with 409 not_stopped unless the server is fully stopped (the world
  // volume is RWO), so callers gate on phase === "Stopped". The path travels as a
  // query parameter — a file path contains "/" and never round-trips through a
  // path segment. Content is []byte on the wire, which Go's encoding/json renders
  // as base64, so it is binary-safe in both directions.
  listServerFiles: (name: string, path: string) =>
    request<{ path: string; entries: ServerFileEntry[]; truncated: boolean }>(
      "GET",
      `/servers/${name}/files?path=${encodeURIComponent(path)}`,
    ),

  // readServerFile returns one file's bytes (base64). A file over the read
  // ceiling is a 413, never a silent truncation, because a later save of a
  // truncated body would destroy the rest of the file.
  readServerFile: (name: string, path: string) =>
    request<{ path: string; content: string }>(
      "GET",
      `/servers/${name}/file?path=${encodeURIComponent(path)}`,
    ),

  // writeServerFile replaces a file's contents (creating it if absent). Sending
  // an explicit "" is a deliberate truncate; the wire field is required, but that
  // is enforced by the caller (this method always sends one).
  writeServerFile: (name: string, path: string, content: string) =>
    request<{ path: string; status: string }>(
      "PUT",
      `/servers/${name}/file?path=${encodeURIComponent(path)}`,
      { content },
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

  // setEmail records the caller's address WITHOUT an OTP round-trip (the setup
  // wizard's Step 1). The bootstrap has no SMTP, so email_verified stays false; a
  // later Settings/SMTP flow verifies it via emailStart/emailVerify.
  setEmail: (email: string) =>
    request<{ email: string }>("POST", "/account/email", { email }),

  passkeyRegisterBegin: () =>
    request<any>("POST", "/account/passkey/register/begin"),

  passkeyRegisterFinish: (name: string, attestation: any) =>
    request<any>("POST", "/account/passkey/register/finish", { name, attestation }),

  passkeyList: () =>
    request<{ credentials: any[] }>("GET", "/account/passkey/credentials"),

  passkeyDelete: (id: string) =>
    request<void>("DELETE", `/account/passkey/credentials/${id}`),

  // Account migration (spec §B3 inherit). Started in-game with /felis migrate; the
  // web side then drives: status → step-up confirm (passkey when enrolled, email-OTP
  // otherwise) → issue-code (source names the target account and reads the one-time
  // code) → redeem (the TARGET account spends the code; the source's servers move to
  // it and the source is retired).
  migrateStatus: () =>
    request<{
      active: boolean;
      state?: string;
      target_user_id?: string;
      confirm_factor?: string;
      code_expires_at?: string;
    }>("GET", "/account/migrate"),

  migrateConfirmOTPStart: () =>
    request<{ sent: boolean; expires_at: string }>("POST", "/account/migrate/confirm/otp/start"),

  migrateConfirmOTPVerify: (code: string) =>
    request<{ confirmed: boolean }>("POST", "/account/migrate/confirm/otp/verify", { code }),

  migrateConfirmPasskeyBegin: () =>
    request<any>("POST", "/account/migrate/confirm/passkey/begin"),

  migrateConfirmPasskeyFinish: (assertion: any) =>
    request<{ confirmed: boolean }>("POST", "/account/migrate/confirm/passkey/finish", {
      assertion,
    }),

  migrateIssueCode: (target_user_id: string) =>
    request<{ code: string; expires_at: string }>("POST", "/account/migrate/issue-code", {
      target_user_id,
    }),

  migrateRedeem: (code: string) =>
    request<{ migrated: boolean; servers_moved: number; servers: string[] }>(
      "POST",
      "/account/migrate/redeem",
      { code },
    ),

  listSubmissions: () =>
    request<{ submissions: Submission[] }>("GET", "/submissions").then((r) => r.submissions ?? []),

  approveSubmission: (id: string) =>
    request<Submission>("POST", `/submissions/${id}/approve`),

  rejectSubmission: (id: string, reason: string) =>
    request<Submission>("POST", `/submissions/${id}/reject`, { reason }),

  // Retire a submission outright (row + uploaded context) — the review queue's
  // lifecycle valve, the only way an upload is reclaimed from the PVC.
  deleteSubmission: (id: string) => request<Submission>("DELETE", `/submissions/${id}`),

  // The reviewer's read path to the uploaded build context: the executed
  // Dockerfile lives inside the tarball, so approving without this would be
  // blind. The body is the attacker-supplied archive — download it, never
  // render it — which the API's attachment disposition enforces.
  downloadSubmissionContext: async (id: string): Promise<void> => {
    const { apiBase } = await loadConfig();
    const res = await fetch(`${apiBase}/submissions/${id}/context`, {
      method: "GET",
      credentials: "include",
    });
    if (!res.ok) {
      let code = "error";
      let message = res.statusText;
      try {
        const parsed = JSON.parse(await res.text()) as unknown;
        if (isApiError(parsed)) {
          code = parsed.error.code;
          message = parsed.error.message;
        }
      } catch {
        /* non-JSON error body (e.g. an ingress page): keep the status line */
      }
      const err: ApiError = { status: res.status, code, message };
      announceSetupRequired(err);
      throw err;
    }
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `${id}-context.tar.gz`;
    link.click();
    URL.revokeObjectURL(url);
  },

  listMySubmissions: () =>
    request<{ submissions: Submission[] }>("GET", "/me/submissions").then((r) => r.submissions ?? []),

  createSubmission: (displayName: string) =>
    request<Submission>("POST", "/me/submissions", { display_name: displayName }),

  uploadSubmissionContext: (id: string, file: Blob) =>
    requestRaw<Submission>("POST", `/me/submissions/${id}/context`, file, {
      "Content-Type": "application/x-gzip",
    }),

  // Retract the caller's own pending submission (and its uploaded context), which
  // frees their pending slot and storage budget. Reviewed submissions are frozen.
  withdrawSubmission: (id: string) => request<Submission>("DELETE", `/me/submissions/${id}`),

  getUpdateWindow: () => request<UpdateWindow>("GET", "/updates/window"),

  setUpdateWindow: (window: UpdateWindow) => request<UpdateWindow>("PUT", "/updates/window", window),

  // ---- User admin (admin-tier, spec §7 user admin) ----

  listUsers: (params?: {
    query?: string;
    role?: "admin" | "user";
    disabled?: "true" | "false";
    limit?: number;
    offset?: number;
  }) => {
    const sp = new URLSearchParams();
    if (params?.query) sp.set("query", params.query);
    if (params?.role) sp.set("role", params.role);
    if (params?.disabled) sp.set("disabled", params.disabled);
    if (params?.limit) sp.set("limit", String(params.limit));
    if (params?.offset) sp.set("offset", String(params.offset));
    const qs = sp.toString();
    return request<{ users: UserView[]; total: number }>(
      "GET",
      `/users${qs ? `?${qs}` : ""}`,
    ).then((r) => ({ users: r.users ?? [], total: r.total ?? 0 }));
  },

  getUser: (id: string) => request<UserDetail>("GET", `/users/${id}`),

  createUser: (req: CreateUserRequest) =>
    request<UserView>("POST", "/users", req),

  patchUser: (id: string, patch: PatchUserRequest) =>
    request<UserView>("PATCH", `/users/${id}`, patch),

  deleteUser: (id: string) =>
    request<{ deleted: boolean }>("DELETE", `/users/${id}`),

  disableUser: (id: string, disabled: boolean) =>
    request<{ id: string; disabled: boolean }>("POST", `/users/${id}/disable`, { disabled }),

  getUserQuotas: (id: string) => request<QuotaView>("GET", `/users/${id}/quotas`),

  setUserQuotas: (id: string, quotas: QuotaInput) =>
    request<QuotaView>("PUT", `/users/${id}/quotas`, quotas),

  listUserSessions: (id: string) =>
    request<{ sessions: SessionView[] }>("GET", `/users/${id}/sessions`).then((r) => r.sessions ?? []),

  revokeUserSessions: (id: string) =>
    request<{ ok: boolean }>("DELETE", `/users/${id}/sessions`),

  revokeUserSession: (id: string, hash: string) =>
    request<{ ok: boolean }>("DELETE", `/users/${id}/sessions/${encodeURIComponent(hash)}`),

  // unbindUserPasskeys severs EVERY passkey the user holds (owner-tier account
  // remediation for a lost or compromised authenticator). It is deliberately not
  // a lockout — the account keeps its other doors (email OTP, in-game op-login
  // re-enrollment). Unbinding an account that holds no passkeys is a 200 no-op.
  unbindUserPasskeys: (id: string) =>
    request<{ ok: boolean }>("DELETE", `/users/${id}/passkeys`),

  linkAccount: (id: string, mcUuid: string, authSource?: string) =>
    request<{ ok: boolean; mc_uuid: string; auth_source: string }>(
      "POST",
      `/users/${id}/links`,
      { mc_uuid: mcUuid, auth_source: authSource ?? "mojang" },
    ),

  unlinkAccount: (id: string, mcUuid: string) =>
    request<{ ok: boolean; mc_uuid: string }>(
      "DELETE",
      `/users/${id}/links/${encodeURIComponent(mcUuid)}`,
    ),
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
  const t = i18next.getFixedT(null, "errors");

  if (e && typeof e === "object" && "name" in e) {
    const name = (e as any).name;
    if (name === "NotAllowedError") {
      return t("passkey_not_allowed");
    }
    if (name === "AbortError") {
      return t("passkey_aborted");
    }
  }

  const err = e as Partial<ApiError>;
  switch (err.code) {
    // Session doors (spec §B): every passwordless door 403s this when local
    // sessions are disabled on a Zero-Trust-only deployment.
    case "local_auth_disabled":
      return t("local_auth_disabled");
    case "staff_account":
      return t("staff_account");
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
    case "no_world_volume":
      return t("no_world_volume");
    case "restore_unavailable":
      return t("restore_unavailable");
    // Server create/edit (spec §22): the portability regex + reservation list are
    // enforced server-side, and the create form's own checks are weaker, so these
    // refusals reach the dialog as-is.
    case "bad_name":
      return t("bad_name");
    case "bad_subdomain":
      return t("bad_subdomain");
    case "at_capacity":
      return t("at_capacity");
    case "storage_immutable":
      return t("storage_immutable");
    // Email identity: the verified-email uniqueness index (migration 0020) plus
    // VerifyEmailOTP's guard make a second verified holder impossible; the OTP
    // relay can also refuse to deliver at all.
    case "email_taken":
      return t("email_taken");
    case "mail_undeliverable":
      return t("mail_undeliverable");
    // File editor (spec §7): path/size refusals from the sandboxed job, plus the
    // subsystem being unwired.
    case "bad_path":
      return t("bad_path");
    case "too_large":
      return t("too_large");
    case "files_timeout":
      return t("files_timeout");
    case "files_unavailable":
      return t("files_unavailable");
    case "jobs_unavailable":
      return t("jobs_unavailable");
    // Builds, uploads and review: terminal-state conflicts and unwired subsystems.
    case "already_terminal":
      return t("already_terminal");
    case "build_unavailable":
      return t("build_unavailable");
    case "build_logs_unavailable":
      return t("build_logs_unavailable");
    case "already_reviewed":
      return t("already_reviewed");
    case "submission_quota_exceeded":
      return t("submission_quota_exceeded");
    case "submission_cooldown":
      return t("submission_cooldown");
    case "submissions_unavailable":
      return t("submissions_unavailable");
    case "uploads_unavailable":
      return t("uploads_unavailable");
    case "backup_unavailable":
      return t("backup_unavailable");
    // Account migration + the re-auth steps it depends on: every refusal below is
    // an expected outcome of the Account → migrate flow, not a fault.
    case "account_retired":
      return t("account_retired");
    case "no_migration":
      return t("no_migration");
    case "not_confirmed":
      return t("not_confirmed");
    case "already_confirmed":
      return t("already_confirmed");
    case "invalid_target":
      return t("invalid_target");
    case "target_not_found":
      return t("target_not_found");
    case "target_unavailable":
      return t("target_unavailable");
    case "no_passkey":
      return t("no_passkey");
    case "passkey_required":
      return t("passkey_required");
    case "no_step_up_factor":
      return t("no_step_up_factor");
    case "passkey_login_failed":
      return t("passkey_login_failed");
    case "passkey_login_invalid":
      return t("passkey_login_invalid");
    case "too_many_challenges":
      return t("too_many_challenges");
    // Operator-login approvals, live streams, and the remaining auth doors.
    case "op_login_invalid":
      return t("op_login_invalid");
    case "op_login_not_found":
      return t("op_login_not_found");
    case "too_many_streams":
      return t("too_many_streams");
    case "protected_admin":
      return t("protected_admin");
    case "auth_unavailable":
      return t("auth_unavailable");
    case "setup_token_invalid":
      return t("setup_token_invalid");
    default:
      if (err.status === 401) return t("session_expired");
      if (err.status === 403) return t("forbidden");
      return err.message ?? t("generic");
  }
}
