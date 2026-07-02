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

/** ServerInfo is the projection returned by GET /servers/{name}/status and
 *  GET /me/servers (the latter wraps a list under { servers: [...] }). */
export interface ServerInfo {
  name: string;
  subdomain: string;
  displayName?: string;
  phase: Phase;
  desiredState?: "Running" | "Stopped";
  players?: number;
  maxPlayers?: number;
  autostartPolicy?: AutostartPolicy;
  /** Whether the caller may claim this server (unowned + linked + quota). */
  claimable?: boolean;
  /** Whether the caller owns it. */
  owned?: boolean;
  image?: string;
  javaMemory?: string;
  storageSize?: string;
  cpu?: string;
}

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
 *  ONLY source of WHO is online — ServerInfo.players carries the count alone.
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
 *  shape — `playersOnline`/`playersMax` (not players/maxPlayers), plus `ready` and
 *  the `endpoint*` runtime fields — whereas ServerInfo is the /me/servers
 *  projection. Sharing one interface would silently read `undefined` across the
 *  fetch().json() boundary for every renamed field. */
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
  role: "user" | "admin";
  is_admin: boolean;
  /** Local-password path only: the account owes a forced first-login password
   *  change. The JWT/Access path always leaves it false. Like `is_admin` it crosses
   *  the untyped fetch().json() boundary, so consumers MUST compare `=== true` — an
   *  absent field is `undefined` (correctly "no change owed"), never a thrown access. */
  must_change_password: boolean;
  email_verified?: boolean;
}

/** LoginResult mirrors POST /api/v1/auth/login (handlers_auth.go handleLogin). The
 *  session cookie is set as a side effect (HttpOnly, so the panel never sees it);
 *  the body carries only what the panel routes on next — chiefly whether to force the
 *  change-password card before any other surface. */
export interface LoginResult {
  user_id: string;
  role: "user" | "admin";
  must_change_password: boolean;
}

export interface BindResult {
  user_id: string;
  linked: boolean;
  mc_uuid: string;
  auth_source: string;
}

export type BuildStatus = "pending" | "building" | "succeeded" | "failed" | "cancelled";

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
  reviewed_by?: string;
  reject_reason?: string;
  created_at: string;
  reviewed_at?: string;
}

export interface UpdateWindow {
  start: string | null;
  end: string | null;
}
