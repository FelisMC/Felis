// Mirror of the felis-api external-face JSON (spec §7). Kept intentionally narrow:
// only the fields the panel renders, so the UI never depends on internal shapes.

// The full lifecycle set felis-api can emit (v1alpha1 MinecraftServerPhase, passed
// through verbatim by the status projection). "Stopping" is the transient teardown
// state between Running and Stopped; the panel must model it or a stopping server
// renders with no variant/color. Consumers that index a phase→x map MUST still
// fall back for any value outside this union — `phase` crosses an unvalidated JSON
// boundary, so the type is a documentation of intent, not a runtime guarantee.
export type Phase =
  | "Stopped"
  | "Starting"
  | "Running"
  | "Stopping"
  | "Failed"
  | "Unknown";

export type AutostartPolicy = "ownerOnly" | "public" | "allowlist";

/** ServerInfo is the GET /me/servers row (wrapped under { servers: [...] }).
 *  Only this projection says whether the caller owns or may claim a server. */
export interface ServerInfo {
  name: string;
  subdomain: string;
  displayName?: string;
  phase: Phase;
  desiredState?: "Running" | "Stopped";
  playersOnline?: number;
  playersMax?: number;
  autostartPolicy?: AutostartPolicy;
  /** Whether the caller may claim this server (unowned + linked + quota). */
  claimable?: boolean;
  /** Whether the caller owns it. */
  owned?: boolean;
  image?: string;
  javaMemory?: string;
  storageSize?: string;
  cpu?: string;
  /** Seconds empty before idle auto-stop; 0 when the server never idles out. */
  idleStopSeconds?: number;
  /** True while the operator cannot read the player count; idle stop waits. */
  playerCountUnknown?: boolean;
}

/** ServerStatus is GET /servers/{name}/status. It never carries `owned` or
 *  `claimable`; owner-tier gates read /me/servers via lib/ownership. */
export type ServerStatus = Omit<ServerInfo, "owned" | "claimable">;

/** WhitelistResult projects GET /servers/{name}/access/whitelist (spec §7 access).
 *  `players` is a BEST-EFFORT parse of the vanilla "whitelist list" reply done
 *  server-side (parseWhitelistOutput); `output` is the raw RCON text and is the
 *  ground truth — on a non-vanilla or localized server the parse may come back
 *  empty while `output` still names players, so the panel falls back to `output`
 *  rather than rendering a falsely-empty list. */
export interface WhitelistResult {
  name: string;
  players: string[];
  output: string;
}

/** BanlistResult projects GET /servers/{name}/access/ban (spec §7 access). Same
 *  shape as WhitelistResult: `players` is a BEST-EFFORT parse of the vanilla
 *  "banlist" reply done server-side (parseBanlistOutput) and `output` is the raw
 *  RCON text — ground truth. A ban entry reads "<name> was banned by <src>: <reason>",
 *  so unlike the whitelist the parse anchors on the ban marker (a reason carries its
 *  own colon); on a non-vanilla or localized server the parse may come back empty
 *  while `output` still names players, so the panel falls back to `output`. */
export interface BanlistResult {
  name: string;
  players: string[];
  output: string;
}

/** AccessResult is the common echo of a successful access mutation (whitelist add/
 *  remove, ban/pardon): the server replays the structured action it ran plus the
 *  raw RCON `output`, which the panel surfaces verbatim as confirmation. */
export interface AccessResult {
  name: string;
  action: string;
  player: string;
  output: string;
}

/** PlayersResult projects GET /servers/{name}/access/players (spec §7 access), the
 *  ONLY source of WHO is online — ServerInfo.playersOnline carries the count alone.
 *  `online`/`max` are the tally; `players` is a BEST-EFFORT parse of the vanilla
 *  "list" reply (parseListOutput) and, like the whitelist, can come back empty on a
 *  non-vanilla format while `output` (the raw RCON text, ground truth) still names
 *  them. `online` can therefore be > `players.length` — show the count, fall back
 *  to `output` for names. */
export interface PlayersResult {
  name: string;
  online: number;
  max: number;
  players: string[];
  output: string;
}

/** KickResult echoes a successful kick. Kick is a single verb (no add/remove), so
 *  unlike AccessResult it carries no `action` — just the player and raw reply. */
export interface KickResult {
  name: string;
  player: string;
  output: string;
}

/** FleetServer is one row of GET /api/v1/fleet — the SysAdmin cockpit's fleet-wide
 *  read (admin-tier). It mirrors the Go fleetServerView: the CRD lifecycle
 *  projection plus the owner joined read-only from Postgres for display.
 *
 *  It is a DISTINCT type from ServerInfo, not a reuse: /fleet emits the raw CRD
 *  shape — `ready` and the `endpoint*` runtime fields, with playersOnline/playersMax
 *  required — whereas ServerInfo is the /me/servers projection with them optional.
 *  Sharing one interface would blur which fields each face actually guarantees. */
export interface FleetServer {
  name: string;
  subdomain: string;
  phase: Phase;
  ready: boolean;
  desiredState?: "Running" | "Stopped";
  autostartPolicy?: AutostartPolicy;
  endpointMode?: string;
  endpointAddress?: string;
  playersOnline: number;
  playersMax: number;
  /** Owner's display identity (email, or username when the address is absent).
   *  Empty/absent for an unclaimed server or when the best-effort owner lookup
   *  failed — the cockpit renders that as "unclaimed". */
  owner?: string;
  /** True for a platform-provisioned system service (the login gate, the lobby).
   *  Their reserved names are rejected by every per-server route, so the cockpit
   *  renders them read-only instead of offering actions that would 400. */
  system?: boolean;
}

/** BackupView is one row of GET /api/v1/backups (spec §7 backups). A backup is
 *  written only when the reaper archives an inactive world's PVC before reclaiming
 *  it (reason "inactive_15d"), so a backup is the SAVED STATE of a world that was
 *  put to sleep: `former_owner` is who owned it then, `expires_at` the §466
 *  retention deadline past which it can no longer be restored. The opaque
 *  backup_ref is deliberately withheld (spec §286) — the panel never names a backup
 *  by handle; restore resolves the latest present backup server-side.
 *
 *  Only `status: "present"` rows are ever listed (the query filters them) and the
 *  list is created_at-descending, so the FIRST row for a given server is exactly
 *  the one a restore would recover (LatestBackup's WHERE mirrors this) — the UI must
 *  name that row, not a plausible proxy. `reason`/`status` cross an unvalidated JSON
 *  boundary; render unknown values tolerantly. */
export interface BackupView {
  id: string;
  server_name: string;
  /** Present only when the world had an owner when it was archived. */
  former_owner?: string;
  size_bytes: number;
  reason: string;
  status: string;
  created_at: string;
  expires_at: string;
}

/** ServerJob is one row of GET /api/v1/servers/{name}/jobs — the observable
 *  outcome of an async backup/restore Job. The API only enqueues Jobs, so this
 *  projection is how a 202 that later failed becomes visible in the panel. */
export interface ServerJob {
  name: string;
  kind: string; // "backup" | "restore"
  state: string; // "running" | "succeeded" | "failed"
  message?: string;
  started_at?: string;
  finished_at?: string;
  /** Set on a restore's safety snapshot (a backup job): "pending" until the
   *  restore behind it starts ("started") or is given up ("abandoned", with a
   *  code in then_restore_reason and its English in message). */
  then_restore?: string;
  then_restore_reason?: string;
  restore_backup_id?: string;
}

/** WhitelistImage is one row of GET /images (the create-form dropdown source). */
export interface WhitelistImage {
  image_ref: string;
  enabled: boolean;
  source?: string;
}

/** CreateServerRequest is the §15 structured form — the ONLY create path. */
export interface CreateServerRequest {
  name: string;
  subdomain: string;
  displayName?: string;
  image: string;
  memory: string;
  storage: string;
  autostartPolicy?: AutostartPolicy;
  resources?: {
    cpu?: string;
    cpuRequest?: string;
    memory?: string;
    memoryRequest?: string;
  };
}

/** LinkStatus projects POST /account/link/start (spec §10): whether the caller's
 *  session is already bound to a Minecraft identity. The endpoint also returns an
 *  API-consumer `instructions` string; the panel renders its own player-facing copy
 *  (the raw string names the REST endpoint), so it is intentionally not modelled. */
export interface LinkStatus {
  linked: boolean;
}

/** LinkResult projects a successful POST /account/link/verify (spec §10): the
 *  account_links binding is written and the now-linked Minecraft UUID is echoed
 *  back (the only place the panel learns the UUID — start never returns it). */
export interface LinkResult {
  linked: boolean;
  mc_uuid: string;
}

/** ApiError is the stable error envelope ({ error: { code, message } }). */
export interface ApiError {
  status: number;
  code: string;
  message: string;
}

/** Identity mirrors GET /api/v1/me (app-tier — every authenticated principal may
 *  read *their own* identity). The keys are snake_case because they mirror the Go
 *  handler's JSON map verbatim (handlers_user.go handleMe → {user_id, email, role,
 *  is_admin}); a camelCase rename here would silently read `undefined` across the
 *  untyped fetch().json() boundary and make every admin look like a non-admin.
 *
 *  `is_admin` is the ONLY field the tier model trusts: it is computed server-side as
 *  Principal.IsAdmin() (Role=="admin" AND ViaAdminAccess — the graded-ZT rule), so
 *  the panel never re-derives admin-ness from `role` alone. `role` is carried for
 *  display only and, like `phase`, is documentation of intent, not a runtime
 *  guarantee — it crosses an unvalidated JSON boundary. */
export interface Identity {
  user_id: string;
  email: string;
  role: "user" | "admin" | "owner";
  is_admin: boolean;
  /** Server-computed Principal.IsOwner() — true only for the platform-level
   *  owner account (one above admin). Owners get user management; admins don't. */
  is_owner: boolean;
  email_verified?: boolean;
}

export interface BindResult {
  user_id: string;
  linked: boolean;
  mc_uuid: string;
  auth_source: string;
}

export type BuildStatus = "pending" | "building" | "succeeded" | "failed" | "cancelled";

/** ServerFileEntry mirrors fileedit.Entry — one row of a world-directory listing
 *  (GET /servers/{name}/files). */
export interface ServerFileEntry {
  name: string;
  size: number;
  is_dir: boolean;
  mod_time: string;
}

/** Build mirrors an image_builds row (spec §6, §16). */
export interface Build {
  id: string;
  image_ref: string;
  status: BuildStatus;
  dockerfile?: string;
  context_ref?: string;
  base_image?: string;
  requested_by: string;
  job_name?: string;
  log_ref?: string;
  error?: string;
  created_at: string;
  finished_at?: string;
}

export type SubmissionStatus = "pending_review" | "approved" | "rejected";

/** Submission mirrors an image_submissions row (spec §6, migration 0002). */
export interface Submission {
  id: string;
  submitted_by: string;
  display_name: string;
  context_ref: string;
  status: SubmissionStatus;
  image_ref?: string;
  build_id?: string;
  build_status?: BuildStatus;
  build_error?: string;
  reviewed_by?: string;
  reject_reason?: string;
  created_at: string;
  reviewed_at?: string;
  /** sha256 of the uploaded context; approval must name it (migration 0025). */
  context_sha256?: string;
}

export interface UpdateWindow {
  start: string | null;
  end: string | null;
}

// ---- Control-plane database backup (internal/api/handlers_dbbackup.go dbBackupView) ----

export type DBBackupLabel = "daily" | "pre-migrate" | "pre-restore" | "manual";

export interface DBBackupRecord {
  at: string;
  name: string;
  label: DBBackupLabel;
  size_bytes: number;
  felis_version?: string;
  schema_version?: number;
  dir: string;
}

export interface DBBackupStatus {
  /** Null until the host has recorded its first backup. */
  last: DBBackupRecord | null;
  /** True when there is no record or it is older than max_age_seconds. */
  stale: boolean;
  max_age_seconds: number;
}

// ---- User admin types (internal/api/repo.go UserView, UserDetail, QuotaView, SessionView) ----

export interface UserView {
  id: string;
  username: string;
  email: string;
  role: "admin" | "user" | "owner";
  disabled: boolean;
  email_verified: boolean;
  server_count: number;
  created_at: string;
  updated_at: string;
}

export interface LinkedAccount {
  mc_uuid: string;
  auth_source: string;
  verified_at: string;
}

export interface UserDetail extends UserView {
  deleted_at?: string | null;
  linked_accounts: LinkedAccount[];
}

/** CreateUserRequest mirrors handlers_users.go createUserRequest — passwordless:
 *  the new account signs in via email-OTP / passkey / bind code, never a password. */
export interface CreateUserRequest {
  username: string;
  email?: string;
  role: "admin" | "user";
}

export interface PatchUserRequest {
  username?: string;
  email?: string;
  role?: "admin" | "user";
}

export interface QuotaView {
  user_id: string;
  max_servers?: number | null;
  max_cpu_milli?: number | null;
  max_memory_mb?: number | null;
  max_storage_gb?: number | null;
}

export interface QuotaInput {
  max_servers?: number | null;
  max_cpu_milli?: number | null;
  max_memory_mb?: number | null;
  max_storage_gb?: number | null;
}

export interface SessionView {
  token_hash: string;
  created_at: string;
  expires_at: string;
  revoked_at?: string | null;
}
