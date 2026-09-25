import type {
  AccessResult,
  ApiError,
  AutostartPolicy,
  BackupView,
  BanlistResult,
  Build,
  BuildScan,
  CreateServerRequest,
  CreateUserRequest,
  FleetServer,
  Identity,
  KickResult,
  LinkResult,
  LinkStatus,
  BindResult,
  PasskeyCredential,
  PatchUserRequest,
  PlayersResult,
  QuotaInput,
  QuotaView,
  ServerFileEntry,
  ServerJob,
  MyServerView,
  ServerStatus,
  SessionView,
  UserDetail,
  UserView,
  WhitelistImage,
  WhitelistResult,
  Submission,
  SubmissionListParams,
  SubmissionPage,
  UpdateWindow,
  DBBackupStatus,
} from "./types";
import { loadConfig } from "./config";
import i18next from "i18next";

// Typed client for the felis-api external face (spec §7). Credentials are sent so
// the upstream Zero-Trust / Access cookie rides along; the panel never holds a
// service token, and the RCON password is never requested (spec §8).

function isApiError(x: unknown): x is { error: { code: string; message: string } } {
  if (typeof x !== "object" || x === null || !("error" in x)) return false;
  const e = (x as { error: unknown }).error;
  return typeof e === "object" && e !== null && typeof (e as { code?: unknown }).code === "string";
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

// A 401 from a protected route means the session ended under the page (it
// expired, or an admin revoked it). TierProvider hears this and re-reads /me,
// and RequireAuth then sends the person to /login with the page to come back
// to. /me is left out because it is that re-read, and /auth/* because the
// sign-in doors run without a session by design.
export const SESSION_EXPIRED_EVENT = "felis:session-expired";

function announceSessionExpired(err: ApiError, path: string): void {
  if (err.status !== 401 || typeof window === "undefined") return;
  if (path === "/me" || path.startsWith("/auth/")) return;
  window.dispatchEvent(new Event(SESSION_EXPIRED_EVENT));
}

// CONNECTION_EVENT reports when requests stop reaching the API (detail.ok =
// false) and when one gets through again (true). A fetch that rejects never
// saw a response: the network is down, or Cloudflare Access redirected the
// call to its cross-origin login page because the Access session expired,
// which fetch reports as the same TypeError. AppShell shows a banner with a
// reload, since only a full page load can go through the Access login.
export const CONNECTION_EVENT = "felis:connection";
let connectionLost = false;

/** isConnectionLost reports whether the last call got no response. */
export function isConnectionLost(): boolean {
  return connectionLost;
}

function reportConnection(ok: boolean): void {
  if (connectionLost === !ok) return;
  connectionLost = !ok;
  if (typeof window === "undefined") return;
  window.dispatchEvent(new CustomEvent(CONNECTION_EVENT, { detail: { ok } }));
}

function networkError(e: unknown): ApiError {
  return {
    status: 0,
    code: "network_error",
    message: e instanceof Error ? e.message : String(e),
  };
}

// urlPath builds an API path from literal text and route values, percent-
// encoding each value as one path segment. Values come from router params and
// form fields, and react-router hands params over decoded, so "%2F" arrives as
// "/": interpolated raw, a crafted link could point a button at another
// endpoint, carrying the viewer's cookie. encodeURIComponent covers "/", "?"
// and "#"; "." and ".." survive it and fetch would still walk the path up, so
// those and "" are refused before anything is sent.
export function urlPath(strings: TemplateStringsArray, ...values: string[]): string {
  let out = strings[0];
  values.forEach((v, i) => {
    const seg = String(v);
    if (seg === "" || seg === "." || seg === "..") {
      const err: ApiError = {
        status: 0,
        code: "bad_path_param",
        message: `refusing ${JSON.stringify(seg)} as a path segment`,
      };
      throw err;
    }
    out += encodeURIComponent(seg) + strings[i + 1];
  });
  return out;
}

// fetchOK performs one call and returns the response when it is 2xx, else
// throws an ApiError that always keeps the HTTP status. A body that is not the
// API's JSON envelope (an HTML 502 or 524 page from the tunnel while the API
// restarts, a bare 503 from the ingress) becomes `upstream_unavailable`, so the
// setup and session branches still see the status and the person reads "try
// again shortly" instead of a JSON parse error.
async function fetchOK(path: string, init: RequestInit): Promise<Response> {
  const { apiBase } = await loadConfig();
  let res: Response;
  try {
    res = await fetch(`${apiBase}${path}`, { ...init, credentials: "include" });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    reportConnection(false);
    throw networkError(e);
  }
  reportConnection(true);
  if (res.ok) return res;

  let parsed: unknown = null;
  try {
    parsed = JSON.parse(await res.text());
  } catch {
    /* no body, or not JSON: an ingress or tunnel answered */
  }
  const err: ApiError = isApiError(parsed)
    ? { status: res.status, code: parsed.error.code, message: parsed.error.message }
    : {
        status: res.status,
        code: res.status >= 500 ? "upstream_unavailable" : "error",
        message: res.statusText,
      };
  announceSetupRequired(err);
  announceSessionExpired(err, path);
  throw err;
}

// send returns a 2xx response's parsed JSON body (null for an empty one).
async function send<T>(path: string, init: RequestInit): Promise<T> {
  const res = await fetchOK(path, init);
  let text: string;
  try {
    text = await res.text();
  } catch (e) {
    reportConnection(false);
    throw networkError(e);
  }
  if (!text) return null as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    const err: ApiError = {
      status: res.status,
      code: "upstream_unavailable",
      message: "the response was not JSON",
    };
    throw err;
  }
}

function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  return send<T>(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
}

function requestRaw<T>(
  method: string,
  path: string,
  body: Blob,
  headers?: Record<string, string>,
): Promise<T> {
  return send<T>(path, { method, headers, body });
}

// rejectingSync turns a synchronous throw inside an api method (urlPath refusing
// a segment) into a rejected promise, so every caller handles it the way it
// handles any failed call.
function rejectingSync<T extends Record<string, unknown>>(methods: T): T {
  const out: Record<string, unknown> = {};
  for (const [key, fn] of Object.entries(methods)) {
    out[key] =
      typeof fn === "function"
        ? (...args: unknown[]) => {
            try {
              return fn(...args);
            } catch (e) {
              return Promise.reject(e);
            }
          }
        : fn;
  }
  return out as T;
}

// submissionPage reads one page of a submission list. Filtering and paging run on
// the server; a status count the API left out reads as zero.
function submissionPage(path: string, params?: SubmissionListParams): Promise<SubmissionPage> {
  const sp = new URLSearchParams();
  if (params?.status) sp.set("status", params.status);
  if (params?.query) sp.set("query", params.query);
  if (params?.limit) sp.set("limit", String(params.limit));
  if (params?.offset) sp.set("offset", String(params.offset));
  const qs = sp.toString();
  return request<Partial<SubmissionPage>>("GET", `${path}${qs ? `?${qs}` : ""}`).then((r) => ({
    submissions: r.submissions ?? [],
    total: r.total ?? 0,
    counts: {
      pending_review: r.counts?.pending_review ?? 0,
      approved: r.counts?.approved ?? 0,
      rejected: r.counts?.rejected ?? 0,
    },
  }));
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

export const api = rejectingSync({
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
    request<{ approved: boolean }>("GET", urlPath`/auth/op-login/status/${id}`),

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
    request<{ servers: MyServerView[] }>("GET", "/me/servers").then((r) => r.servers ?? []),

  // fleet is the SysAdmin cockpit's fleet-wide read (admin-tier GET /fleet): every
  // server's CRD lifecycle view plus its owner. It 403s for a non-admin principal —
  // the panel only renders the cockpit link behind is_admin, and the route guards
  // again server-side regardless of what the UI shows.
  fleet: () =>
    request<{ servers: FleetServer[] }>("GET", "/fleet").then((r) => r.servers ?? []),

  status: (name: string) => request<ServerStatus>("GET", urlPath`/servers/${name}/status`),

  wake: (name: string) =>
    request<{ name: string; desiredState: string }>("POST", urlPath`/servers/${name}/wake`),

  stop: (name: string) =>
    request<{ name: string; desiredState: string }>("POST", urlPath`/servers/${name}/stop`),

  claim: (name: string) =>
    request<{ name: string; claimed: boolean }>("POST", urlPath`/servers/${name}/claim`),

  /** sendCommand runs one RCON command against a running server (spec §8 写=RCON).
   *  The backend strips a leading "/", rejects control characters (newline → 400)
   *  and caps the command at 1000 bytes. The reply is the server's plain-text
   *  response body. */
  sendCommand: (name: string, command: string) =>
    request<{ output: string }>("POST", urlPath`/servers/${name}/command`, { command }),

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
    request<WhitelistResult>("GET", urlPath`/servers/${name}/access/whitelist`),

  accessWhitelist: (name: string, action: "add" | "remove", player: string) =>
    request<AccessResult>("POST", urlPath`/servers/${name}/access/whitelist`, {
      action,
      player,
    }),

  /** accessBanList reads the server's ban list. Like accessWhitelistList this GET
   *  requires a Running server (the readiness gate covers the read too), so callers
   *  gate the fetch on phase === "Running". */
  accessBanList: (name: string) =>
    request<BanlistResult>("GET", urlPath`/servers/${name}/access/ban`),

  accessBan: (name: string, action: "ban" | "pardon", player: string) =>
    request<AccessResult>("POST", urlPath`/servers/${name}/access/ban`, {
      action,
      player,
    }),

  /** accessPlayers reads WHO is online (the only source of names — status carries
   *  the count alone). Like accessWhitelistList this GET requires a Running server,
   *  so callers gate the fetch on phase === "Running". */
  accessPlayers: (name: string) =>
    request<PlayersResult>("GET", urlPath`/servers/${name}/access/players`),

  accessKick: (name: string, player: string) =>
    request<KickResult>("POST", urlPath`/servers/${name}/access/kick`, { player }),

  accessLuckPermsInfo: (name: string, player: string) =>
    request<{
      player: string;
      groups: string[];
      permissions: { node: string; value: boolean; world?: string }[];
      output: string;
    }>("GET", urlPath`/servers/${name}/access/luckperms/${player}`),

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
      urlPath`/servers/${name}/access/permission`,
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
      urlPath`/servers/${name}/access/group`,
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

  /** One page of the build history, newest first; rows leave out the Dockerfile. */
  listBuilds: (params?: { query?: string; limit?: number; offset?: number }) => {
    const sp = new URLSearchParams();
    if (params?.query) sp.set("query", params.query);
    if (params?.limit) sp.set("limit", String(params.limit));
    if (params?.offset) sp.set("offset", String(params.offset));
    const qs = sp.toString();
    return request<{ builds: Build[]; total: number }>(
      "GET",
      `/images/build${qs ? `?${qs}` : ""}`,
    ).then((r) => ({ builds: r.builds ?? [], total: r.total ?? 0 }));
  },

  getBuild: (id: string) =>
    request<Build>("GET", urlPath`/images/build/${id}`),

  cancelBuild: (id: string) =>
    request<Build>("POST", urlPath`/images/build/${id}/cancel`),

  /** What the build's scan gate kept; 404 scan_not_found before the scan step. */
  getBuildScan: (id: string) =>
    request<BuildScan>("GET", urlPath`/images/build/${id}/scan`),

  /** Saves the build's full Trivy report or CycloneDX SBOM. The bytes name the
   *  image's own packages and paths, so they are downloaded, never rendered. */
  downloadBuildScanDocument: async (id: string, doc: "report" | "sbom") => {
    const res = await fetchOK(
      doc === "report" ? urlPath`/images/build/${id}/scan/report` : urlPath`/images/build/${id}/sbom`,
      { method: "GET" },
    );
    const url = URL.createObjectURL(await res.blob());
    const link = document.createElement("a");
    link.href = url;
    link.download = doc === "report" ? `${id}-trivy.json` : `${id}.cdx.json`;
    link.click();
    URL.revokeObjectURL(url);
  },

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
    /** Required with an image that moves the server to another build: the world is
     *  opened by that build's Minecraft version, which cannot be undone. */
    confirmImageChange?: boolean;
    memory?: string;
    /** Idle auto-stop: 0 turns it off, else seconds empty before the stop (60–86400). */
    idleStopSeconds?: number;
    resources?: {
      cpu?: string;
      cpuRequest?: string;
      memory?: string;
      memoryRequest?: string;
    };
  }) =>
    request<{ name: string; desiredState: string }>(
      "PATCH",
      urlPath`/servers/${name}`,
      req,
    ),

  // World backups (spec §7). listBackups is the app-tier read: an admin sees every
  // present backup, a user only the backups of worlds they formerly owned — the
  // scope is decided server-side from the principal, so a user cannot widen it.
  // server narrows the page to one server inside that scope. Only present
  // (restorable) rows come back, newest first, one page at a time with the total.
  listBackups: (params?: { server?: string; limit?: number; offset?: number }) => {
    const sp = new URLSearchParams();
    if (params?.server) sp.set("server", params.server);
    if (params?.limit) sp.set("limit", String(params.limit));
    if (params?.offset) sp.set("offset", String(params.offset));
    const qs = sp.toString();
    return request<{ backups: BackupView[]; total: number }>("GET", `/backups${qs ? `?${qs}` : ""}`).then(
      (r) => ({ backups: r.backups ?? [], total: r.total ?? 0 }),
    );
  },

  // restoreBackup starts an ASYNC restore of a server's world from a backup
  // (spec §7 POST restore-backup). It accepts an optional backupId in the body: when
  // absent the backend restores the latest backup and resolves its opaque ref
  // server-side — the client never names a backup by handle (spec §286).
  // Preconditions are enforced server-side and surfaced as codes: owner-or-admin +
  // former-owner match (403), a present backup must exist (404 no_backup), and the
  // server MUST be fully stopped (409 not_stopped) since the restore writes into
  // the live world volume. By default the backend first backs up the world as it
  // is (a "pre_restore" backup) and starts the restore only once that succeeded;
  // safetySnapshot=false skips it. The reply is 202 {name, status:"restoring",
  // backup_id, safety_snapshot} — success means the work was enqueued, not that
  // the world is back yet; serverJobs shows the snapshot and the restore.
  restoreBackup: (name: string, backupId?: string, safetySnapshot = true) => {
    const body: { backup_id?: string; safety_snapshot?: boolean } = {};
    if (backupId) body.backup_id = backupId;
    if (!safetySnapshot) body.safety_snapshot = false;
    return request<{ name: string; status: string; backup_id: string; safety_snapshot?: boolean }>(
      "POST",
      urlPath`/servers/${name}/restore-backup`,
      Object.keys(body).length ? body : undefined,
    );
  },

  // backupNow enqueues a manual backup (spec §7 POST backup). Preconditions are
  // enforced server-side and surfaced as codes: owner-or-admin (403) and the
  // server MUST be fully stopped (409 not_stopped — the world volume is RWO), so
  // callers gate the action on phase === "Stopped". The reply is 202
  // {name, status:"backing_up"}: the Job is enqueued, not done — watch
  // serverJobs for the outcome.
  backupNow: (name: string) =>
    request<{ name: string; status: string }>("POST", urlPath`/servers/${name}/backup`),

  // serverJobs lists the newest backup/restore Jobs of one server, newest first
  // (GET /servers/{name}/jobs). Owner-or-admin gated server-side; a Job's
  // failure text rides `message`. The backend answers 503 until the job-status
  // reader is wired, so callers should tolerate that error.
  serverJobs: (name: string) =>
    request<{ server: string; jobs: ServerJob[] }>(
      "GET",
      urlPath`/servers/${name}/jobs`,
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
      urlPath`/servers/${name}/files` + `?path=${encodeURIComponent(path)}`,
    ),

  // readServerFile returns one file's bytes (base64) and the sha256 of the file
  // as stored. A file over the read ceiling is a 413, never a silent truncation,
  // because a later save of a truncated body would destroy the rest of the file.
  readServerFile: (name: string, path: string) =>
    request<{ path: string; content: string; sha256: string }>(
      "GET",
      urlPath`/servers/${name}/file` + `?path=${encodeURIComponent(path)}`,
    ),

  // writeServerFile atomically replaces a file's contents (creating it if
  // absent). Sending an explicit "" is a deliberate truncate; the wire field is
  // required, but that is enforced by the caller (this method always sends one).
  // With expectSha256 (the hash the read returned) a file someone changed since
  // is refused with 409 file_changed; without it the write is unconditional.
  writeServerFile: (name: string, path: string, content: string, expectSha256?: string) =>
    request<{ path: string; status: string; sha256: string }>(
      "PUT",
      urlPath`/servers/${name}/file` + `?path=${encodeURIComponent(path)}`,
      expectSha256 ? { content, expect_sha256: expectSha256 } : { content },
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
    request<{ credentials: PasskeyCredential[] }>("GET", "/account/passkey/credentials"),

  passkeyDelete: (id: string) =>
    request<void>("DELETE", urlPath`/account/passkey/credentials/${id}`),

  // The caller's own sessions: every browser signed in to the account, the one
  // making the request marked current. Revoking the current one is a sign-out.
  listMySessions: () =>
    request<{ sessions: SessionView[] }>("GET", "/account/sessions").then((r) => r.sessions ?? []),

  revokeMySession: (hash: string) =>
    request<{ ok: boolean; signed_out: boolean }>("DELETE", urlPath`/account/sessions/${hash}`),

  revokeMyOtherSessions: () =>
    request<{ revoked: number }>("POST", "/account/sessions/revoke-others"),

  // Re-authentication. Adding or removing a passkey and changing the email are
  // refused with 403 reauth_required unless this session proved a factor in the
  // last few minutes. Status names the factors that can prove it: "passkey",
  // "email" (a code to the verified address) or "sign_in" (operators sign in
  // again). Like the migrate begin, the passkey begin returns the raw
  // {"publicKey": {...}} document.
  reauthStatus: () =>
    request<{ needed: boolean; until?: string; factors: string[] }>("GET", "/account/reauth"),

  reauthPasskeyBegin: () => request<any>("POST", "/account/reauth/passkey/begin"),

  reauthPasskeyFinish: (assertion: any) =>
    request<{ ok: boolean; until: string }>("POST", "/account/reauth/passkey/finish", { assertion }),

  reauthEmailStart: () =>
    request<{ sent: boolean; expires_at: string }>("POST", "/account/reauth/email/start"),

  reauthEmailVerify: (code: string) =>
    request<{ ok: boolean; until: string }>("POST", "/account/reauth/email/verify", { code }),

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

  // The admin review queue, one page at a time (see submissionPage).
  listSubmissions: (params?: SubmissionListParams) => submissionPage("/submissions", params),

  // expectedDigest is the sha256 of the context the reviewer looked at; the API
  // refuses the approval (409 context_changed) when the upload has since changed.
  approveSubmission: (id: string, expectedDigest: string) =>
    request<Submission>("POST", urlPath`/submissions/${id}/approve`, { expected_digest: expectedDigest }),

  rejectSubmission: (id: string, reason: string) =>
    request<Submission>("POST", urlPath`/submissions/${id}/reject`, { reason }),

  // Retire a submission outright (row + uploaded context) — the review queue's
  // lifecycle valve, the only way an upload is reclaimed from the PVC.
  deleteSubmission: (id: string) => request<Submission>("DELETE", urlPath`/submissions/${id}`),

  // The reviewer's read path to the uploaded build context: the executed
  // Dockerfile lives inside the tarball, so approving without this would be
  // blind. The body is the attacker-supplied archive — download it, never
  // render it — which the API's attachment disposition enforces.
  // Resolves to the sha256 the API vouched for while streaming these bytes (it
  // aborts the transfer on a mismatch), so the approval can name what was read.
  downloadSubmissionContext: async (id: string): Promise<string | null> => {
    const res = await fetchOK(urlPath`/submissions/${id}/context`, { method: "GET" });
    const digest = res.headers.get("X-Felis-Context-Sha256")?.trim().toLowerCase() || null;
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `${id}-context.tar.gz`;
    link.click();
    URL.revokeObjectURL(url);
    return digest;
  },

  listMySubmissions: (params?: SubmissionListParams) => submissionPage("/me/submissions", params),
  /** The per-upload context cap, checked before a file is sent. */
  submissionLimits: () => request<{ max_context_bytes: number }>("GET", "/me/submissions/limits"),

  createSubmission: (displayName: string) =>
    request<Submission>("POST", "/me/submissions", { display_name: displayName }),

  uploadSubmissionContext: (id: string, file: Blob) =>
    requestRaw<Submission>("POST", urlPath`/me/submissions/${id}/context`, file, {
      "Content-Type": "application/x-gzip",
    }),

  // Retract the caller's own pending submission (and its uploaded context), which
  // frees their pending slot and storage budget. Reviewed submissions are frozen.
  withdrawSubmission: (id: string) => request<Submission>("DELETE", urlPath`/me/submissions/${id}`),

  getUpdateWindow: () => request<UpdateWindow>("GET", "/updates/window"),

  setUpdateWindow: (window: UpdateWindow) => request<UpdateWindow>("PUT", "/updates/window", window),

  // Freshness of the host's control-plane database backup (felis-db-backup.timer).
  getDBBackup: () => request<DBBackupStatus>("GET", "/platform/db-backup"),

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

  getUser: (id: string) => request<UserDetail>("GET", urlPath`/users/${id}`),

  createUser: (req: CreateUserRequest) =>
    request<UserView>("POST", "/users", req),

  patchUser: (id: string, patch: PatchUserRequest) =>
    request<UserView>("PATCH", urlPath`/users/${id}`, patch),

  deleteUser: (id: string) =>
    request<{ deleted: boolean }>("DELETE", urlPath`/users/${id}`),

  disableUser: (id: string, disabled: boolean) =>
    request<{ id: string; disabled: boolean }>("POST", urlPath`/users/${id}/disable`, { disabled }),

  getUserQuotas: (id: string) => request<QuotaView>("GET", urlPath`/users/${id}/quotas`),

  setUserQuotas: (id: string, quotas: QuotaInput) =>
    request<QuotaView>("PUT", urlPath`/users/${id}/quotas`, quotas),

  listUserSessions: (id: string) =>
    request<{ sessions: SessionView[] }>("GET", urlPath`/users/${id}/sessions`).then((r) => r.sessions ?? []),

  revokeUserSessions: (id: string) =>
    request<{ ok: boolean }>("DELETE", urlPath`/users/${id}/sessions`),

  revokeUserSession: (id: string, hash: string) =>
    request<{ ok: boolean }>("DELETE", urlPath`/users/${id}/sessions/${hash}`),

  // unbindUserPasskeys severs EVERY passkey the user holds (owner-tier account
  // remediation for a lost or compromised authenticator). It is deliberately not
  // a lockout — the account keeps its other doors (email OTP, in-game op-login
  // re-enrollment). Unbinding an account that holds no passkeys is a 200 no-op.
  unbindUserPasskeys: (id: string) =>
    request<{ ok: boolean }>("DELETE", urlPath`/users/${id}/passkeys`),

  linkAccount: (id: string, mcUuid: string, authSource?: string) =>
    request<{ ok: boolean; mc_uuid: string; auth_source: string }>(
      "POST",
      urlPath`/users/${id}/links`,
      { mc_uuid: mcUuid, auth_source: authSource ?? "mojang" },
    ),

  unlinkAccount: (id: string, mcUuid: string) =>
    request<{ ok: boolean; mc_uuid: string }>(
      "DELETE",
      urlPath`/users/${id}/links/${mcUuid}`,
    ),
});

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

/** clientError is a failure the panel itself detects (no request was made),
 *  shaped like a server error so humanizeError words it from the code. */
export function clientError(code: string): ApiError {
  return { status: 0, code, message: "" };
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
    case "otp_account_locked":
      return t("otp_account_locked");
    // Volumetric limits on the sign-in doors (internal/api/ratelimit.go): one
    // network calling too fast, or the install-wide mail budget spent.
    case "rate_limited":
      return t("rate_limited");
    case "mail_rate_limited":
      return t("mail_rate_limited");
    case "passkey_challenge_invalid":
      return t("passkey_challenge_invalid");
    case "invalid_attestation":
      return t("invalid_attestation");
    case "passkey_already_bound":
      return t("passkey_already_bound");
    case "passkey_unavailable":
      return t("passkey_unavailable");
    case "last_passkey":
      return t("last_passkey");
    // User admin (internal/api/handlers_users.go): the caller's own account and
    // the owner account are refused, each for its own reason.
    case "self_protected":
      return t("self_protected");
    case "owner_protected":
      return t("owner_protected");
    case "session_not_found":
      return t("session_not_found");
    case "quota_exceeded":
      return t("quota_exceeded");
    case "already_claimed":
      return t("already_claimed");
    case "image_not_whitelisted":
      return t("image_not_whitelisted");
    // Image pinning (internal/imagepin): a server runs the exact build its tag
    // named when it was created or last changed, so the registry has to hold the
    // tag, and moving a world to another build needs an explicit confirmation.
    case "image_not_in_registry":
      return t("image_not_in_registry");
    case "registry_unavailable":
      return t("registry_unavailable");
    case "image_change_unconfirmed":
      return t("image_change_unconfirmed");
    case "subdomain_taken":
      return t("subdomain_taken");
    case "already_exists":
      return t("already_exists");
    // A server deleted by hand left its world volume; the name stays taken so a
    // new server cannot mount the old world.
    case "world_volume_exists":
      return t("world_volume_exists");
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
    // The backup failed a read-back (sha256 or gzip/tar parse), so the server
    // refuses to extract it over the world.
    case "backup_corrupt":
      return t("backup_corrupt");
    case "not_stopped":
      return t("not_stopped");
    // World-volume lock: a restore, backup or file write is running on this
    // server's world, so a wake or a second world operation is refused until the
    // Job finishes (internal/maintenance).
    case "maintenance_in_progress":
      return t("maintenance_in_progress");
    case "no_world_volume":
      return t("no_world_volume");
    // File editor: the file changed after it was opened (another manager saved
    // it, or the server rewrote it), so the save was refused rather than
    // overwriting that edit.
    case "file_changed":
      return t("file_changed");
    case "volume_full":
      return t("volume_full");
    // On-demand backup rationing (data-durability-9): one per server per
    // cooldown, none while the shared backup store is at its cap.
    case "backup_cooldown":
      return t("backup_cooldown");
    case "backup_store_full":
      return t("backup_store_full");
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
    case "bad_idle_stop":
      return t("bad_idle_stop");
    // Email identity: the verified-email uniqueness index (migration 0020) plus
    // VerifyEmailOTP's guard make a second verified holder impossible; the OTP
    // relay can also refuse to deliver at all.
    case "email_taken":
      return t("email_taken");
    case "mail_undeliverable":
      return t("mail_undeliverable");
    // No [smtp] relay at all: every door that mails a code refuses before minting.
    case "mail_unavailable":
      return t("mail_unavailable");
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
    case "context_changed":
      return t("context_changed");
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
    // Re-authentication before a change to how the account signs in.
    case "reauth_required":
      return t("reauth_required");
    case "staff_reauth":
      return t("staff_reauth");
    case "no_session":
      return t("no_session");
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
    // The request never got a response: the network is down, or Cloudflare
    // Access sent it to its login page because the Access session expired.
    case "network_error":
      return t("network_error");
    // A proxy or the tunnel answered for the API (it is restarting or down).
    case "upstream_unavailable":
      return t("upstream_unavailable");
    case "bad_path_param":
      return t("bad_path_param");
    case "passkey_no_credential":
      return t("passkey_no_credential");
    case "not_found":
      return t("not_found");
    case "conflict":
      return t("conflict");
    case "restore_in_progress":
      return t("restore_in_progress");
    // 507: named on its own so the 5xx fallback below does not call it an
    // outage.
    case "uploads_full":
      return t("uploads_full");
    case "unsupported_media_type":
      return t("unsupported_media_type");
    // The detail says which field was wrong; the server writes it in English,
    // so it rides inside a localized sentence.
    case "bad_request":
      return err.message ? t("bad_request", { detail: err.message }) : t("generic");
    default:
      if (err.status === 401) return t("session_expired");
      if (err.status === 403) return t("forbidden");
      if (err.status === 413) return t("payload_too_large");
      if (err.status !== undefined && err.status >= 500) return t("upstream_unavailable");
      return err.message ?? t("generic");
  }
}
