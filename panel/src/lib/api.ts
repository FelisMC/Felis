import type {
  HostOverview,
  PlatformUsage,
  ServerUsage,
  ConsoleHistoryPage,
  ConsoleCommandResult,
  NodeControlRequest,
  NodeControlTask,
  AccessResult,
  ExecutionNode,
  WorldMigration,
  AllowlistEntry,
  ApiError,
  AutostartPolicy,
  BackupView,
  BanlistResult,
  Build,
  BuildScan,
  ContextUploadProgress,
  CreateServerRequest,
  SelfCreateRequest,
  SelfServiceQuotaSettings,
  ServerNotify,
  ServerAssignment,
  ServerPowerResult,
  ServerTags,
  ServerStartupSettings,
  ServerExtraPortRequest,
  StartupSettingsPatch,
  ServerQuota,
  SoftwareRequest,
  SoftwareType,
  SoftwareVersion,
  SoftwareVersionInfo,
  CreateUserRequest,
  ExportStatus,
  ExportTicket,
  FileOp,
  FileUploadSession,
  FleetServer,
  Identity,
  KickResult,
  LinkResult,
  MinecraftAuthSource,
  AuthSourceConfig,
  AuthSourcesSettings,
  MailSettings,
  MailSettingsRequest,
  WakePolicySettings,
  DefaultLanguageSettings,
  PanelLanguage,
  BrandingImageKind,
  BrandingSettings,
  PublicBranding,
  APITokenList,
  APITokenSettings,
  CreatedAPIToken,
  FailoverPolicySettings,
  ArchivePolicy,
  ArchivePolicySettings,
  AuditListParams,
  AuditPage,
  HeartbeatSettings,
  FailoverPlan,
  FailoverResult,
  EntryPolicySettings,
  MinecraftProfile,
  LinkStatus,
  BindResult,
  PasskeyCredential,
  PatchUserRequest,
  PlayersResult,
  QuotaInput,
  QuotaView,
  RetireState,
  Schedule,
  ScheduleInput,
  ServerFileEntry,
  ServerExtensionsListing,
  PaperGlobalConfig,
  PaperGlobalValues,
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
  UpdateReport,
} from "./types";
import { loadConfig } from "./config";
import { base64Sha256, sha256Of } from "./digest";
import i18next from "i18next";

async function verifyContentDigest(content: string, expected: string): Promise<void> {
  let got: string | undefined;
  try {
    got = await base64Sha256(content);
  } catch {
    // Malformed base64 is reported as a damaged transfer.
  }
  if (!got || got !== expected) throw clientError("read_damaged");
}

// Send session cookies; the panel never receives service tokens or RCON passwords.

function isApiError(x: unknown): x is { error: { code: string; message: string; request_id?: string } } {
  if (typeof x !== "object" || x === null || !("error" in x)) return false;
  const e = (x as { error: unknown }).error;
  return typeof e === "object" && e !== null && typeof (e as { code?: unknown }).code === "string";
}

// Announce setup_required so the router can resume passkey enrollment.
export const SETUP_REQUIRED_EVENT = "felis:setup-required";

function announceSetupRequired(err: ApiError): void {
  if (err.status !== 403 || err.code !== "setup_required") return;
  if (typeof window === "undefined") return;
  window.dispatchEvent(new Event(SETUP_REQUIRED_EVENT));
}

// Recheck identity after protected-route 401s; exclude /me and unauthenticated sign-in endpoints.
export const SESSION_EXPIRED_EVENT = "felis:session-expired";

function announceSessionExpired(err: ApiError, path: string): void {
  if (err.status !== 401 || typeof window === "undefined") return;
  if (path === "/me" || path.startsWith("/auth/")) return;
  window.dispatchEvent(new Event(SESSION_EXPIRED_EVENT));
}

// Recheck identity after role-gate 403s to remove access after demotion.
export const ACCESS_REFUSED_EVENT = "felis:access-refused";
const ROLE_REFUSALS = new Set(["forbidden", "not_admin"]);

function announceAccessRefused(err: ApiError): void {
  if (err.status !== 403 || !ROLE_REFUSALS.has(err.code) || typeof window === "undefined") return;
  window.dispatchEvent(new Event(ACCESS_REFUSED_EVENT));
}

// Rejected fetches cover network outages and cross-origin Access redirects alike.
// AppShell shows a banner with a reload, since only a full page load can go through the Access login.
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

// Encode individual path segments and reject empty, dot, and parent segments before authenticated fetches.
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

// Preserve HTTP status even for non-JSON upstream errors.
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

  let text = "";
  try {
    text = await res.text();
  } catch {
    /* the body broke off; the status still says what happened */
  }
  throw failed(path, res.status, res.statusText, text);
}

// failed turns a non-2xx answer into the ApiError the callers see and announces
// the codes the app shell acts on.
function failed(path: string, status: number, statusText: string, text: string): ApiError {
  let parsed: unknown = null;
  try {
    parsed = JSON.parse(text);
  } catch {
    /* no body, or not JSON: an ingress or tunnel answered */
  }
  const err: ApiError = isApiError(parsed)
    ? { status, code: parsed.error.code, message: parsed.error.message, ...(typeof parsed.error.request_id === "string" && { request_id: parsed.error.request_id }) }
    : {
        status,
        code: status >= 500 ? "upstream_unavailable" : "error",
        message: statusText,
      };
  announceSetupRequired(err);
  announceSessionExpired(err, path);
  announceAccessRefused(err);
  return err;
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

// Fetch attachments through the client so reauth_required can open the verification dialog.
async function downloadBlob(path: string): Promise<{ blob: Blob; filename: string }> {
  const res = await fetchOK(path, { method: "GET" });
  const disposition = res.headers.get("Content-Disposition") ?? "";
  const filename = /filename="([^"]+)"/.exec(disposition)?.[1] ?? "felis-download";
  return { blob: await res.blob(), filename };
}

function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  return send<T>(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
}

// XMLHttpRequest supplies upload progress; cancellation rejects with AbortError.
async function sendWithProgress<T>(
  method: string,
  path: string,
  body: Blob,
  opts: { onProgress?: (sent: number) => void; signal?: AbortSignal; headers?: Record<string, string> } = {},
): Promise<T> {
  const { apiBase } = await loadConfig();
  const { onProgress, signal, headers } = opts;
  return new Promise<T>((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const onAbort = () => xhr.abort();
    const settle = () => signal?.removeEventListener("abort", onAbort);
    xhr.open(method, `${apiBase}${path}`);
    xhr.withCredentials = true;
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    for (const [k, v] of Object.entries(headers ?? {})) xhr.setRequestHeader(k, v);
    if (onProgress) xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onabort = () => {
      settle();
      reject(new DOMException("The upload was cancelled", "AbortError"));
    };
    xhr.onerror = () => {
      settle();
      reportConnection(false);
      reject(networkError(new Error("the upload did not reach the API")));
    };
    xhr.onload = () => {
      settle();
      reportConnection(true);
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(failed(path, xhr.status, xhr.statusText, xhr.responseText));
        return;
      }
      try {
        resolve((xhr.responseText ? JSON.parse(xhr.responseText) : null) as T);
      } catch {
        const err: ApiError = {
          status: xhr.status,
          code: "upstream_unavailable",
          message: "the response was not JSON",
        };
        reject(err);
      }
    };
    if (signal?.aborted) {
      reject(new DOMException("The upload was cancelled", "AbortError"));
      return;
    }
    signal?.addEventListener("abort", onAbort);
    xhr.send(body);
  });
}

// Attach Content-Digest and reject mismatched bodies before storing any bytes.
async function sendDigested<T>(
  method: string,
  path: string,
  body: Blob,
  opts: { onProgress?: (sent: number) => void; signal?: AbortSignal } = {},
): Promise<T> {
  const { header } = await sha256Of(body);
  return sendWithProgress<T>(method, path, body, { ...opts, headers: { "Content-Digest": header } });
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

// Setup token redemption creates a restricted session until onboarding completes.
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
  // Sign-in endpoints set HttpOnly cookies and may reject local_auth_disabled.

  // Logout is idempotent; reloading /me after clearing the session returns 401.
  logout: () => request<{ ok: boolean }>("POST", "/auth/logout"),

  bind: (code: string) =>
    request<BindResult>("POST", "/auth/bind", { code }),

  // Whether `felis setup` has bound an Owner yet. Until it has, every door above
  // answers 403 local_auth_disabled, so the sign-in page explains that instead.
  authOwnerStatus: () =>
    request<{ owner_bound: boolean }>("GET", "/auth/owner-status"),

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

  // Staff sign-in requires emailed OTP plus in-game approval before session redemption.
  // start answers 202 with a request_id for any well-formed address (anti-enumeration), so the UI just waits.
  opLoginStart: (email: string) =>
    request<{ request_id: string; expires_at: string }>("POST", "/auth/op-login/start", { email }),

  opLoginStatus: (id: string) =>
    request<{ approved: boolean }>("GET", urlPath`/auth/op-login/status/${id}`),

  opLoginFinish: (request_id: string, code: string) =>
    request<{ user_id: string; role: string }>("POST", "/auth/op-login/finish", {
      request_id,
      code,
    }),

  setupRedeem: (token: string) =>
    request<SetupState>("POST", "/auth/setup/redeem", { token }),

  setupStatus: () =>
    request<SetupState>("GET", "/auth/setup/status"),

  // Navigation uses server-computed is_admin; backend routes enforce authorization independently.
  me: () => request<Identity>("GET", "/me"),

  myServers: () =>
    request<{ servers: MyServerView[] }>("GET", "/me/servers").then((r) => r.servers ?? []),

  fleet: () =>
    request<{ servers: FleetServer[] }>("GET", "/fleet").then((r) => r.servers ?? []),

  fleetUsage: () => request<PlatformUsage>("GET", "/fleet/usage"),

  emergencyStop: (name: string, confirm: string) => send<{ name: string; desiredState: string }>(urlPath`/servers/${name}/emergency-stop`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ confirm }), signal: AbortSignal.timeout(20_000) }),
  status: (name: string) => send<ServerStatus>(urlPath`/servers/${name}/status`, { method: "GET", signal: AbortSignal.timeout(12_000) }),
  serverUsage: (name: string) => send<ServerUsage>(urlPath`/servers/${name}/usage`, { method: "GET", signal: AbortSignal.timeout(12_000) }),
  getStartupSettings: (name: string) => request<ServerStartupSettings>("GET", urlPath`/servers/${name}/startup-settings`),

  wake: (name: string) =>
    request<ServerPowerResult>("POST", urlPath`/servers/${name}/wake`),

  stop: (name: string) =>
    request<ServerPowerResult>("POST", urlPath`/servers/${name}/stop`),

  restart: (name: string) =>
    request<ServerPowerResult>("POST", urlPath`/servers/${name}/restart`),

  kill: (name: string) =>
    request<ServerPowerResult>("POST", urlPath`/servers/${name}/kill`),

  claim: (name: string) =>
    request<{ name: string; claimed: boolean }>("POST", urlPath`/servers/${name}/claim`),

  // Retirement confirmation repeats the server name; only administrators can request deletion.
  // The cancel is the owner's for a give-up and an admin's for a deletion.
  retireServer: (name: string, body: { confirm: string; delete: boolean }) =>
    request<{ name: string; retiring: RetireState }>("PUT", urlPath`/servers/${name}/retirement`, body),

  cancelRetire: (name: string) => request<void>("DELETE", urlPath`/servers/${name}/retirement`),

  // Replaces the whole tag set; the response carries the set as stored.
  setServerTags: (name: string, tags: string[]) =>
    request<ServerTags>("PUT", urlPath`/servers/${name}/tags`, { tags }),

  // Owner only. The preview names the current owner and the quota caps the account would pass.
  previewServerAssignment: (name: string, userId: string) =>
    request<ServerAssignment>("GET", urlPath`/servers/${name}/assignment` + `?user_id=${encodeURIComponent(userId)}`),

  assignServer: (name: string, body: { user_id: string; allow_over_quota: boolean }) =>
    request<ServerAssignment>("POST", urlPath`/servers/${name}/assignment`, body),

  // The server strips a leading slash and rejects control chars or >1024 bytes; the line is typed into the server console, so its output arrives in the log.
  sendCommand: (name: string, command: string) =>
    request<ConsoleCommandResult>("POST", urlPath`/servers/${name}/command`, { command, console: true }),

  serverConsoleHistory: async (name: string, cursor?: string) => {
    const page = await request<ConsoleHistoryPage>("GET", urlPath`/servers/${name}/console/history` + (cursor ? `?cursor=${encodeURIComponent(cursor)}` : ""));
    await verifyContentDigest(page.content, page.content_sha256);
    return page;
  },

  // Structured access operations require a running server; the backend validates command fields.
  // The reply's output is the raw RCON text, surfaced verbatim as confirmation.

  // Whitelist reads also require phase Running.
  accessWhitelistList: (name: string) =>
    request<WhitelistResult>("GET", urlPath`/servers/${name}/access/whitelist`),

  accessWhitelist: (name: string, action: "add" | "remove", player: string) =>
    request<AccessResult>("POST", urlPath`/servers/${name}/access/whitelist`, {
      action,
      player,
    }),

  /** accessBanList reads the ban list. Like accessWhitelistList it requires a Running server, so callers gate the fetch on phase === "Running". */
  accessBanList: (name: string) =>
    request<BanlistResult>("GET", urlPath`/servers/${name}/access/ban`),

  accessBan: (name: string, action: "ban" | "pardon", player: string) =>
    request<AccessResult>("POST", urlPath`/servers/${name}/access/ban`, {
      action,
      player,
    }),

  // Player names come from RCON, while lifecycle status provides only counts.
  accessPlayers: (name: string) =>
    request<PlayersResult>("GET", urlPath`/servers/${name}/access/players`),

  accessKick: (name: string, player: string) =>
    request<KickResult>("POST", urlPath`/servers/${name}/access/kick`, { player }),

  // The wake allowlist is independent of the game's whitelist and works while stopped.
  serverAllowlist: (name: string) =>
    request<{ server: string; entries: AllowlistEntry[] }>(
      "GET",
      urlPath`/servers/${name}/allowlist`,
    ).then((r) => r.entries ?? []),

  setAllowlistWake: (name: string, mcUUID: string, canWake: boolean) =>
    request<void>("PUT", urlPath`/servers/${name}/allowlist/${mcUUID}`, { can_wake: canWake }),

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

  nodeTasks: () => request<{ available: boolean; tasks: NodeControlTask[] }>("GET", "/settings/node-control"),
  nodeTask: (id: string) => request<NodeControlTask>("GET", `/settings/node-control/tasks/${encodeURIComponent(id)}`),
  startNodeTask: (body: NodeControlRequest) => request<NodeControlTask>("POST", "/settings/node-control/tasks", body),
  retryNodeTask: (id: string) => request<NodeControlTask>("POST", `/settings/node-control/tasks/${encodeURIComponent(id)}/retry`),
  nodeTaskArtifact: (id: string) => downloadBlob(urlPath`/settings/node-control/tasks/${id}/artifact`),
  hostOverview: () => request<HostOverview>("GET", "/platform/host"),
  holdAlerts: (until: string | null) => request<null>("POST", "/platform/host/quiet", { until }),
  revealOffsiteKey: () => request<{ key: string }>("POST", "/platform/offsite/key"),
  exportAudit: (since: string, until: string) => {
    const q = new URLSearchParams();
    if (since) q.set("since", since);
    if (until) q.set("until", until);
    const query = q.toString();
    return downloadBlob(`/platform/audit-export${query ? `?${query}` : ""}`);
  },
  listAudit: (params: AuditListParams = {}) => {
    const sp = new URLSearchParams();
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== "") sp.set(key, String(value));
    }
    const qs = sp.toString();
    return request<AuditPage>("GET", `/platform/audit${qs ? `?${qs}` : ""}`);
  },
  nodes: () => request<{ nodes: ExecutionNode[] }>("GET", "/nodes").then((r) => r.nodes),
  migration: (name: string) => request<WorldMigration>("GET", `/servers/${encodeURIComponent(name)}/migrations`),
  migrateServer: (name: string, targetNode: string) =>
    request<WorldMigration>("POST", `/servers/${encodeURIComponent(name)}/migrations`, { targetNode }),
  retryMigration: (name: string, id: string) =>
    request<WorldMigration>("POST", `/servers/${encodeURIComponent(name)}/migrations/${encodeURIComponent(id)}/retry`),
  // Gives up a failed migration: the server stays on its source world and node.
  abortMigration: (name: string, id: string) =>
    request<WorldMigration>("POST", `/servers/${encodeURIComponent(name)}/migrations/${encodeURIComponent(id)}/abort`),
  // What failing a lost worker over would do, and the failover itself (Owner): each
  // server comes back on targetNode from its newest intact backup.
  failoverPlan: (node: string) => request<FailoverPlan>("GET", `/nodes/${encodeURIComponent(node)}/failover`),
  failOverNode: (node: string, targetNode: string, confirm: string) =>
    request<FailoverResult>("POST", `/nodes/${encodeURIComponent(node)}/failover`, { targetNode, confirm }),

  createServer: (req: CreateServerRequest) =>
    request<{ name: string; subdomain: string; desiredState: string }>(
      "POST",
      "/servers",
      req,
    ),

  softwareCatalog: () =>
    request<{ types: { type: SoftwareType; builds: boolean }[]; runtimes: number[] }>("GET", "/software"),
  softwareVersions: (type: SoftwareType) =>
    request<{ versions: SoftwareVersion[] }>("GET", urlPath`/software/${type}/versions`).then((r) => r.versions ?? []),
  softwareVersion: (type: SoftwareType, version: string) =>
    request<SoftwareVersionInfo>("GET", urlPath`/software/${type}/versions/${version}`),

  // Self-service (internal/api/selfservice.go): the caller's quota, a server of
  // their own, and the changes an owner may make to one.
  myServerQuota: () => request<ServerQuota>("GET", "/me/server-quota"),
  createMyServer: (req: SelfCreateRequest) =>
    request<{ name: string; subdomain: string; desiredState: string }>("POST", "/me/servers", req),
  patchMyServer: (name: string, req: StartupSettingsPatch & {
    extraPorts?: ServerExtraPortRequest[] | null;
    displayName?: string;
    autostartPolicy?: AutostartPolicy;
    idleStopSeconds?: number;
    memory?: string;
    storage?: string;
    cpu?: string;
    software?: SoftwareRequest;
    confirmSoftwareChange?: boolean;
    acceptEula?: boolean;
    resumeAfterHostRestart?: boolean;
    restartOnCrash?: boolean;
    maxCrashRestarts?: number;
    notify?: Partial<ServerNotify>;
  }) => request<{ name: string; patched: string[] }>("PATCH", urlPath`/me/servers/${name}`, req),

  // The Owner's default quota: the caps of every user without one of their own.
  getSelfServiceQuota: () => request<SelfServiceQuotaSettings>("GET", "/settings/self-service"),
  setSelfServiceQuota: (body: Omit<SelfServiceQuotaSettings, "revision"> & { revision: string }) =>
    request<SelfServiceQuotaSettings>("PUT", "/settings/self-service", body),

  patchServer: (name: string, req: StartupSettingsPatch & {
    extraPorts?: ServerExtraPortRequest[] | null;
    /** Trimmed server-side; an empty one clears it. */
    displayName?: string;
    autostartPolicy?: AutostartPolicy;
    image?: string;
    /** Required with an image that moves the server to another build: the world is
     *  opened by that build's Minecraft version, which cannot be undone. */
    confirmImageChange?: boolean;
    memory?: string;
    /** The world's storage quota; it cannot drop to or below the space already used. */
    storage?: string;
    /** Idle auto-stop: 0 turns it off, else seconds empty before the stop (60–86400). */
    idleStopSeconds?: number;
    /** Moves the server onto (other) server software; not together with image. */
    software?: SoftwareRequest;
    /** Required for another type or Minecraft version, or leaving a server image. */
    confirmSoftwareChange?: boolean;
    /** Required when leaving a server image for server software. */
    acceptEula?: boolean;
    /** Single fields laid over the server's pod block: a field left out keeps its
     *  value, and an empty cpu or cpuRequest removes that limit or request. */
    resources?: {
      cpu?: string;
      cpuRequest?: string;
      memory?: string;
      memoryRequest?: string;
    };
    resumeAfterHostRestart?: boolean;
    restartOnCrash?: boolean;
    /** Crashes in a row restarted before the server stops (1–20). */
    maxCrashRestarts?: number;
    /** Only the given switches change. */
    notify?: Partial<ServerNotify>;
  }) =>
    request<{ name: string; patched: string[] }>(
      "PATCH",
      urlPath`/servers/${name}`,
      req,
    ),

  // Paginate present backups within the backend's administrator/former-owner scope.
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

  // Deleting a backup queues archive removal; active restores reject with restore_in_progress.
  deleteBackup: (id: string) =>
    request<{ id: string; status: "expired" }>("DELETE", urlPath`/backups/${id}`),

  // Restore selects the latest backup by default and queues after its safety snapshot succeeds.
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

  // 202 means the manual backup is queued; follow serverJobs for completion.
  backupNow: (name: string) =>
    request<{ name: string; status: string }>("POST", urlPath`/servers/${name}/backup`),

  // Export tickets are single-use, expire after 90 seconds ready, and are subject to per-user and platform limits.
  startBackupExport: (name: string, id: string) =>
    request<ExportTicket>("POST", urlPath`/servers/${name}/backups/${id}/export`),

  startWorldExport: (name: string) =>
    request<ExportTicket>("POST", urlPath`/servers/${name}/world/export`),

  exportStatus: (ticket: string) => request<ExportStatus>("GET", urlPath`/exports/${ticket}`),

  exportDownloadURL: async (ticket: string) => {
    const { apiBase } = await loadConfig();
    return `${apiBase}${urlPath`/exports/${ticket}/download`}`;
  },

  // Job status may return 503 when its reader is unavailable.
  serverJobs: (name: string) =>
    request<{ server: string; jobs: ServerJob[] }>(
      "GET",
      urlPath`/servers/${name}/jobs`,
    ).then((r) => r.jobs ?? []),

  listSchedules: (name: string) =>
    request<{ server: string; schedules: Schedule[]; limit: number }>(
      "GET",
      urlPath`/servers/${name}/schedules`,
    ).then((r) => ({ schedules: r.schedules ?? [], limit: r.limit })),

  // createSchedule saves a new schedule for the server's current owner. Validation failures are 400 bad_request or bad_schedule, with the reason in the message.
  createSchedule: (name: string, input: ScheduleInput) =>
    request<Schedule>("POST", urlPath`/servers/${name}/schedules`, input),

  // Updating replaces the whole schedule and transfers it to the current owner.
  updateSchedule: (name: string, id: number, input: ScheduleInput) =>
    request<Schedule>("PUT", urlPath`/servers/${name}/schedules/${String(id)}`, input),

  // deleteSchedule answers 204; 409 schedule_running while it runs.
  deleteSchedule: (name: string, id: number) =>
    request<null>("DELETE", urlPath`/servers/${name}/schedules/${String(id)}`),

  // Manual runs preserve the next scheduled time; restart/backup progress continues asynchronously.
  runSchedule: (name: string, id: number) =>
    request<Schedule>("POST", urlPath`/servers/${name}/schedules/${String(id)}/run`),

  // File paths use query parameters and binary contents use base64; listings include free space.
  listServerFiles: (name: string, path: string) =>
    request<{ path: string; entries: ServerFileEntry[]; truncated: boolean; free_bytes: number | null }>(
      "GET",
      urlPath`/servers/${name}/files` + `?path=${encodeURIComponent(path)}`,
    ),

  listServerExtensions: (name: string) =>
    request<ServerExtensionsListing>("GET", urlPath`/servers/${name}/extensions`),

  serverPaperGlobal: (name: string) =>
    request<PaperGlobalConfig>("GET", urlPath`/servers/${name}/config/paper-global`),

  patchServerPaperGlobal: (name: string, values: PaperGlobalValues, expect: string) =>
    request<PaperGlobalConfig>("PATCH", urlPath`/servers/${name}/config/paper-global`, { values, expect_sha256: expect }),

  searchServerFiles: (name: string, path: string, query: string) =>
    request<{ path: string; entries: ServerFileEntry[]; truncated: boolean; free_bytes: number | null }>(
      "GET", urlPath`/servers/${name}/files/search` + `?path=${encodeURIComponent(path)}&q=${encodeURIComponent(query)}`,
    ),

  transferServerFiles: (name: string, op: "copy" | "move", paths: string[], to: string, overwrite = false) =>
    request<{ op: FileOp }>("POST", urlPath`/servers/${name}/files/transfer`, { op, paths, to, overwrite }),

  zipServerFiles: (name: string, paths: string[], to: string, overwrite = false) =>
    request<{ op: FileOp }>("POST", urlPath`/servers/${name}/files/zip`, { paths, to, overwrite }),

  fetchServerFile: (name: string, url: string, path: string, overwrite = false) =>
    request<{ op: FileOp }>("POST", urlPath`/servers/${name}/files/from-url`, { url, path, overwrite }),

  // Reject oversized or digest-damaged reads so subsequent saves cannot truncate or corrupt the original.
  readServerFile: async (name: string, path: string) => {
    const r = await request<{ path: string; content: string; sha256: string; content_sha256: string }>(
      "GET",
      urlPath`/servers/${name}/file` + `?path=${encodeURIComponent(path)}`,
    );
    await verifyContentDigest(r.content, r.content_sha256);
    return r;
  },

  // An empty body truncates; verify content digest and expected prior hash before atomic replacement.
  writeServerFile: async (name: string, path: string, content: string, expectSha256?: string) =>
    request<{ path: string; status: string; sha256: string }>(
      "PUT",
      urlPath`/servers/${name}/file` + `?path=${encodeURIComponent(path)}`,
      {
        content,
        content_sha256: await base64Sha256(content),
        ...(expectSha256 ? { expect_sha256: expectSha256 } : {}),
      },
    ),

  // createServerFile makes a new file with content, and only if nothing is at the
  // path yet: something that appeared meanwhile is 409 file_exists, never replaced.
  createServerFile: async (name: string, path: string, content: string) =>
    request<{ path: string; status: string; sha256: string }>(
      "PUT",
      urlPath`/servers/${name}/file` + `?path=${encodeURIComponent(path)}`,
      { content, content_sha256: await base64Sha256(content), create_only: true },
    ),

  // deleteServerFile deletes a file, a link (never what it names) or a folder with
  // everything in it. The route does not ask; the page confirms first.
  deleteServerFile: (name: string, path: string) =>
    request<{ path: string; status: string }>(
      "DELETE",
      urlPath`/servers/${name}/file` + `?path=${encodeURIComponent(path)}`,
    ),

  // mkdirServerFolder makes one folder in an existing parent (404 otherwise);
  // anything already at the path is 409 file_exists.
  mkdirServerFolder: (name: string, path: string) =>
    request<{ path: string; status: string }>(
      "POST",
      urlPath`/servers/${name}/files/mkdir` + `?path=${encodeURIComponent(path)}`,
    ),

  // renameServerFile moves path to `to`, never over something: an existing `to`
  // is 409 file_exists. The files felis manages refuse with 400 bad_path.
  renameServerFile: (name: string, path: string, to: string) =>
    request<{ path: string; to: string; status: string }>(
      "POST",
      urlPath`/servers/${name}/files/rename` + `?path=${encodeURIComponent(path)}`,
      { to },
    ),

  // Uploads replace a file atomically only when overwrite is requested.
  uploadServerFile: (
    name: string,
    path: string,
    file: Blob,
    overwrite: boolean,
    opts?: { onProgress?: (sent: number) => void; signal?: AbortSignal },
  ) =>
    sendDigested<{ path: string; status: string; sha256: string; size: number }>(
      "PUT",
      urlPath`/servers/${name}/files/upload` +
        `?path=${encodeURIComponent(path)}` +
        (overwrite ? "&overwrite=true" : ""),
      file,
      opts,
    ),

  // Reserve total upload space, send digest-checked parts by offset, then commit asynchronously.
  // Sessions remain reusable after landing failure and expire after six hours idle.
  beginServerFileUpload: (name: string, path: string, size: number) =>
    request<FileUploadSession>(
      "POST",
      urlPath`/servers/${name}/files/uploads` + `?path=${encodeURIComponent(path)}`,
      { size },
    ),

  getServerFileUpload: (name: string, id: string) =>
    request<FileUploadSession>("GET", urlPath`/servers/${name}/files/uploads/${id}`),

  putServerFileUploadPart: (
    name: string,
    id: string,
    offset: number,
    part: Blob,
    opts?: { onProgress?: (sent: number) => void; signal?: AbortSignal },
  ) =>
    sendDigested<FileUploadSession>(
      "PUT",
      urlPath`/servers/${name}/files/uploads/${id}` + `?offset=${offset}`,
      part,
      opts,
    ),

  deleteServerFileUpload: (name: string, id: string) =>
    request<null>("DELETE", urlPath`/servers/${name}/files/uploads/${id}`),

  commitServerFileUpload: (name: string, id: string, overwrite: boolean) =>
    request<{ op: FileOp }>("POST", urlPath`/servers/${name}/files/uploads/${id}/commit`, { overwrite }),

  // Extraction conflicts require confirmation before retrying with overwrite.
  unzipServerFile: (name: string, path: string, overwrite: boolean) =>
    request<{ op: FileOp }>(
      "POST",
      urlPath`/servers/${name}/files/unzip` + `?path=${encodeURIComponent(path)}`,
      { overwrite },
    ),

  // listServerFileOps is the server's running op, if any, and those that ended
  // within the last 30 minutes, newest first.
  listServerFileOps: (name: string) =>
    request<{ ops: FileOp[] }>("GET", urlPath`/servers/${name}/files/ops`),

  // downloadServerFile starts an export of one file, or of a folder as a zip,
  // and answers its ticket (exportStatus, then exportDownloadURL). The world is
  // held until the download ends.
  downloadServerFile: (name: string, path: string, dir: boolean) =>
    request<ExportTicket>(
      "POST",
      urlPath`/servers/${name}/files/download` + `?path=${encodeURIComponent(path)}&dir=${dir}`,
    ),

  downloadServerFiles: (name: string, paths: string[]) =>
    request<ExportTicket>("POST", urlPath`/servers/${name}/files/download`, { paths }),

  previewServerFile: async (ticket: string, maxBytes: number, signal?: AbortSignal) => {
    const response = await fetchOK(urlPath`/exports/${ticket}/download`, { method: "GET", signal });
    const reader = response.body?.getReader();
    if (!reader) throw clientError("read_damaged");
    const chunks: Uint8Array<ArrayBuffer>[] = [];
    let size = 0;
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        size += value.length;
        if (size > maxBytes) throw clientError("too_large");
        chunks.push(new Uint8Array(value));
      }
      return new Blob(chunks);
    } finally {
      await reader.cancel();
    }
  },

  // Link codes originate in-game; the web client can only redeem them.
  linkStatus: () => request<LinkStatus>("POST", "/account/link/start"),

  linkVerify: (code: string) =>
    request<LinkResult>("POST", "/account/link/verify", { code }),

  linkSources: () => request<{ sources: MinecraftAuthSource[] }>("GET", "/account/link/sources"),

  getEntryPolicy: () => request<EntryPolicySettings>("GET", "/settings/entry-policy"),
  setEntryPolicy: (policy: EntryPolicySettings) => request<EntryPolicySettings>("PUT", "/settings/entry-policy", policy),

  getWakePolicy: () => request<WakePolicySettings>("GET", "/settings/wake-policy"),
  setWakePolicy: (policy: Omit<WakePolicySettings, "managed">) => request<WakePolicySettings>("PUT", "/settings/wake-policy", policy),
  getDefaultLanguage: () => request<DefaultLanguageSettings>("GET", "/settings/default-language"),
  setDefaultLanguage: (settings: DefaultLanguageSettings) => request<DefaultLanguageSettings>("PUT", "/settings/default-language", settings),
  getPanelLanguage: () => request<PanelLanguage>("GET", "/panel/language"),
  getBranding: () => request<PublicBranding>("GET", "/branding"),
  getBrandingSettings: () => request<BrandingSettings>("GET", "/settings/branding"),
  setBrandingSettings: (settings: Omit<BrandingSettings, "has_logo" | "has_background">) => request<BrandingSettings>("PUT", "/settings/branding", settings),
  // The image goes up as the request body; its own type is the Content-Type the server checks.
  uploadBrandingImage: (kind: BrandingImageKind, image: File) => send<BrandingSettings>(`/settings/branding/${kind}`, { method: "PUT", headers: { "Content-Type": image.type }, body: image }),
  removeBrandingImage: (kind: BrandingImageKind) => request<BrandingSettings>("DELETE", `/settings/branding/${kind}`),
  getAPITokenSettings: () => request<APITokenSettings>("GET", "/settings/api-tokens"),
  setAPITokenSettings: (settings: APITokenSettings) => request<APITokenSettings>("PUT", "/settings/api-tokens", settings),
  getHeartbeat: () => request<HeartbeatSettings>("GET", "/settings/heartbeat"),
  setHeartbeat: (url: string, revision: string) => request<HeartbeatSettings>("PUT", "/settings/heartbeat", { url, revision }),
  clearHeartbeat: () => request<HeartbeatSettings>("DELETE", "/settings/heartbeat"),
  getArchivePolicy: () => request<ArchivePolicySettings>("GET", "/settings/archive-policy"),
  setArchivePolicy: (policy: ArchivePolicy & { revision: string }) => request<ArchivePolicySettings>("PUT", "/settings/archive-policy", policy),
  clearArchivePolicy: () => request<ArchivePolicySettings>("DELETE", "/settings/archive-policy"),
  getFailoverPolicy: () => request<FailoverPolicySettings>("GET", "/settings/failover"),
  setFailoverPolicy: (policy: Omit<FailoverPolicySettings, "managed">) => request<FailoverPolicySettings>("PUT", "/settings/failover", policy),
  // The mail relay (Owner). Saving proves the relay first; clearing hands it back to
  // felis.toml; a test mails the caller without saving.
  getMailSettings: () => request<MailSettings>("GET", "/settings/smtp"),
  setMailSettings: (body: MailSettingsRequest) => request<MailSettings>("PUT", "/settings/smtp", body),
  clearMailSettings: () => request<MailSettings>("DELETE", "/settings/smtp"),
  testMailSettings: (body: MailSettingsRequest) => request<{ sent_to: string }>("POST", "/settings/smtp/test", body),
  getAuthSources: () => request<AuthSourcesSettings>("GET", "/settings/auth-sources"),
  setAuthSources: (sources: AuthSourceConfig[], revision: string) => request<AuthSourcesSettings>("PUT", "/settings/auth-sources", { sources, revision }),
  testAuthSource: (source: AuthSourceConfig) => request<{ ok: boolean; status: number; elapsed_ms: number }>("POST", "/settings/auth-sources/test", source),

  lookupProfile: (source: string, profile: string) =>
    request<MinecraftProfile>("GET", `/account/link/profile?${new URLSearchParams({ source, profile })}`),

  linkProfile: (source: string, profile_uuid: string) =>
    request<LinkResult>("POST", "/account/link/profile", { source, profile_uuid }),

  emailStart: (email: string) =>
    request<{ sent: boolean; expires_at: string }>("POST", "/account/email/start", { email }),

  emailVerify: (code: string) =>
    request<{ verified: boolean; email: string }>("POST", "/account/email/verify", { code }),

  // Recording an email does not verify it; SMTP verification is a separate step.
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

  listAPITokens: () => request<APITokenList>("GET", "/account/api-tokens"),

  createAPIToken: (body: { name: string; expires_at?: string; admin?: boolean }) =>
    request<CreatedAPIToken>("POST", "/account/api-tokens", body),

  revokeAPIToken: (id: string) => request<void>("DELETE", urlPath`/account/api-tokens/${id}`),

  // Passkey and email changes require a recently verified factor; otherwise return reauth_required.
  // The passkey begin returns the raw {"publicKey": ...} document.
  reauthStatus: () =>
    request<{ needed: boolean; until?: string; factors: string[] }>("GET", "/account/reauth"),

  reauthPasskeyBegin: () => request<any>("POST", "/account/reauth/passkey/begin"),

  reauthPasskeyFinish: (assertion: any) =>
    request<{ ok: boolean; until: string }>("POST", "/account/reauth/passkey/finish", { assertion }),

  reauthEmailStart: () =>
    request<{ sent: boolean; expires_at: string }>("POST", "/account/reauth/email/start"),

  reauthEmailVerify: (code: string) =>
    request<{ ok: boolean; until: string }>("POST", "/account/reauth/email/verify", { code }),

  // Account migration starts in-game and uses step-up confirmation before source-code issue and target redemption.
  migrateStatus: () =>
    request<{
      active: boolean;
      state?: string;
      target_user_id?: string;
      confirm_factor?: string;
      confirm_expires_at?: string;
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

  // Download attacker-controlled build contexts as attachments; never render their contents.
  // It resolves to the sha256 the API vouched for while streaming, so the approval can name what was read.
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

  // Send context parts by byte offset with SHA-256; retry digest_mismatch parts.
  getContextUpload: (id: string) =>
    request<ContextUploadProgress>("GET", urlPath`/me/submissions/${id}/context/upload`),

  putContextPart: (
    id: string,
    offset: number,
    part: Blob,
    opts?: { onProgress?: (sent: number) => void; signal?: AbortSignal },
  ) =>
    sendDigested<ContextUploadProgress>(
      "PUT",
      urlPath`/me/submissions/${id}/context/upload` + `?offset=${offset}`,
      part,
      opts,
    ),

  completeContextUpload: (id: string) =>
    request<Submission>("POST", urlPath`/me/submissions/${id}/context/upload/complete`),

  // Retract the caller's own pending submission (and its uploaded context), which
  // frees their pending slot and storage budget. Reviewed submissions are frozen.
  withdrawSubmission: (id: string) => request<Submission>("DELETE", urlPath`/me/submissions/${id}`),

  getUpdateWindow: () => request<UpdateWindow>("GET", "/updates/window"),

  setUpdateWindow: (window: UpdateWindow) => request<UpdateWindow>("PUT", "/updates/window", window),

  // Freshness of the host's control-plane database backup (felis-db-backup.timer).
  getDBBackup: () => request<DBBackupStatus>("GET", "/platform/db-backup"),
  getUpdateReport: () => request<UpdateReport>("GET", "/updates/report"),


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
  // clearUserQuotas removes the user's own quota, returning them to the default.
  clearUserQuotas: (id: string) => request<QuotaView>("DELETE", urlPath`/users/${id}/quotas`),

  listUserSessions: (id: string) =>
    request<{ sessions: SessionView[] }>("GET", urlPath`/users/${id}/sessions`).then((r) => r.sessions ?? []),

  revokeUserSessions: (id: string) =>
    request<{ ok: boolean }>("DELETE", urlPath`/users/${id}/sessions`),

  revokeUserSession: (id: string, hash: string) =>
    request<{ ok: boolean }>("DELETE", urlPath`/users/${id}/sessions/${hash}`),

  // Unbinding passkeys preserves other sign-in methods and succeeds if none exist.
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

// EventSource uses the encoded endpoint with withCredentials to carry session cookies.
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
    case "server_transitioning":
      return t("server_transitioning");
    // Session doors: every passwordless door 403s this when local sessions are disabled on a Zero-Trust-only deployment.
    case "node_operation_busy":
      return t("node_operation_busy");
    case "node_control_unavailable":
      return t("node_control_unavailable");
    case "distributed_unavailable":
      return t("distributed_unavailable");
    case "node_not_lost":
      return t("node_not_lost");
    case "bad_heartbeat_url":
      return t("bad_heartbeat_url");
    case "bad_image":
      return t("bad_image");
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
    case "bad_mc_uuid":
      return t("bad_mc_uuid");
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
    // Assigning a server to an account (owner only).
    case "already_owner":
      return t("already_owner");
    case "unknown_user":
      return t("unknown_user");
    case "image_not_whitelisted":
      return t("image_not_whitelisted");
    // Changing a pinned server image requires confirmation and a registry-held digest.
    case "image_not_in_registry":
      return t("image_not_in_registry");
    case "registry_unavailable":
      return t("registry_unavailable");
    case "image_change_unconfirmed":
      return t("image_change_unconfirmed");
    case "eula_required":
    case "software_unavailable":
    case "software_not_found":
    case "bad_software":
    case "java_too_old":
    case "java_unavailable":
    case "runtime_unavailable":
    case "software_metadata_unavailable":
    case "software_change_unconfirmed":
    case "self_service_disabled":
    case "no_node":
    case "bad_api_token":
    case "api_tokens_disabled":
    case "api_token_limit":
      return t(err.code);
    case "subdomain_taken":
      return t("subdomain_taken");
    case "already_exists":
      return t("already_exists");
    // A server deleted by hand left its world volume; the name stays taken so a
    // new server cannot mount the old world.
    case "world_volume_exists":
      return t("world_volume_exists");
    // Pending retirement prevents wake or claim; system servers cannot retire.
    case "server_retiring":
      return t("server_retiring");
    case "confirm_mismatch":
      return t("confirm_mismatch");
    case "system_server":
      return t("system_server");
    case "world_volume_orphaned":
      return t("world_volume_orphaned");
    case "cooldown":
      return t("cooldown");
    // Access control: the server must be Running for any RCON-backed access change; the panel gates on phase, but a stale phase can still race.
    case "not_running":
      return t("not_running");
    case "console_unavailable":
      return t("console_unavailable");
    case "command_outcome_unknown":
      return t("command_outcome_unknown");
    case "luckperms_missing":
      return t("luckperms_missing");
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
    case "export_busy":
      return t("export_busy");
    case "export_expired":
      return t("export_expired");
    case "export_not_ready":
      return t("export_not_ready");
    case "export_unavailable":
      return t("export_unavailable");
    case "bad_name":
      return t("bad_name");
    case "bad_subdomain":
      return t("bad_subdomain");
    case "bad_display_name":
      return t("bad_display_name");
    case "bad_startup_settings":
      return t("bad_startup_settings");
    case "at_capacity":
      return t("at_capacity");
    case "storage_below_usage":
      return t("storage_below_usage");
    case "bad_idle_stop":
      return t("bad_idle_stop");
    // Email identity: the verified-email uniqueness index and VerifyEmailOTP's guard make a second verified holder impossible; the OTP relay can also refuse to deliver at all.
    case "email_taken":
      return t("email_taken");
    case "mail_undeliverable":
      return t("mail_undeliverable");
    // No [smtp] relay at all: every door that mails a code refuses before minting.
    case "mail_unavailable":
      return t("mail_unavailable");
    // File editor: path and size refusals from the sandboxed job, plus the subsystem being unwired.
    case "bad_path":
      return t("bad_path");
    case "too_large":
      return t("too_large");
    case "bad_url":
      return t("bad_url");
    case "download_failed":
      return t("download_failed");
    case "files_timeout":
      return t("files_timeout");
    case "files_unavailable":
      return t("files_unavailable");
    // File manager: a create, folder, rename or upload never replaces what is
    // already at the path (unless an upload asked to).
    case "file_exists":
      return t("file_exists");
    // Uploads are staged on felis-api's disk before the file Job lands them: that
    // disk can be at its floor (507), the body can stop short of its length, or a
    // client can send none.
    case "upload_staging_full":
      return t("upload_staging_full");
    case "upload_incomplete":
      return t("upload_incomplete");
    case "length_required":
      return t("length_required");
    // Every upload body carries its SHA-256 (sendDigested): bytes that hash differently on arrival are refused, and a file the browser cannot read is never sent.
    case "digest_mismatch":
      return t("digest_mismatch");
    case "digest_required":
      return t("digest_required");
    case "bad_digest":
      return t("bad_digest");
    case "file_unreadable":
      return t("file_unreadable");
    // A file opened in the editor whose bytes arrived damaged (readServerFile).
    case "read_damaged":
      return t("read_damaged");
    // An upload sent in parts: the session is gone (cancelled, landed, idle for
    // 6 hours, or felis-api restarted), or the account holds four already.
    case "upload_not_found":
      return t("upload_not_found");
    case "too_many_uploads":
      return t("too_many_uploads");
    // Client-side: the op being watched dropped out of the ops list.
    case "op_lost":
      return t("op_lost");
    case "jobs_unavailable":
      return t("jobs_unavailable");
    // Builds, uploads and review: terminal-state conflicts and unwired subsystems.
    case "already_terminal":
      return t("already_terminal");
    case "build_unavailable":
      return t("build_unavailable");
    case "build_logs_unavailable":
      return t("build_logs_unavailable");
    case "scan_document_not_kept":
      return t("scan_document_not_kept");
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
    // Chunked uploads: the client resumes on these by itself, so they surface
    // only once its retries run out.
    case "upload_busy":
      return t("upload_busy");
    case "upload_offset_mismatch":
      return t("upload_offset_mismatch");
    case "uploads_store_unavailable":
      return t("uploads_store_unavailable");
    case "part_too_large":
      return t("part_too_large");
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
    case "migrate_quota_exceeded":
      return t("migrate_quota_exceeded");
    case "invalid_quota":
      return t("invalid_quota");
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
    // The mail relay (internal/api/mail_settings.go): why a relay check failed, in a
    // few words the Owner can act on.
    case "auth_sources_changed":
    case "auth_source_tag_retired":
    case "mail_settings_changed":
    case "mail_settings_unavailable":
    case "smtp_unreachable":
    case "smtp_tls":
    case "smtp_auth":
    case "smtp_sender_refused":
    case "smtp_recipient_refused":
    case "smtp_failed":
      return t(err.code);
    // Scheduled tasks (internal/api/handlers_schedules.go).
    case "schedules_unavailable":
      return t("schedules_unavailable");
    case "schedule_limit":
      return t("schedule_limit");
    case "schedule_running":
      return t("schedule_running");
    case "schedule_stale":
      return t("schedule_stale");
    // The detail says which field was wrong; the server writes it in English,
    // so it rides inside a localized sentence.
    case "bad_request":
      return err.message ? t("bad_request", { detail: err.message }) : t("generic");
    case "bad_schedule":
      return err.message ? t("bad_schedule", { detail: err.message }) : t("generic");
    case "internal":
      return t("internal_error") + (err.request_id ? " " + t("request_id", { id: err.request_id }) : "");
    default:
      if (err.status === 401) return t("session_expired");
      if (err.status === 403) return t("forbidden");
      if (err.status === 413) return t("payload_too_large");
      if (err.status !== undefined && err.status >= 500) return t("upstream_unavailable");
      return err.message ?? t("generic");
  }
}
