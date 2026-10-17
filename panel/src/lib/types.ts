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

/** RetireState is a pending retirement (Go RetireState): the owner gave the
 *  server up, or with `delete` an admin is deleting it. The reaper carries it out
 *  on its next daily run (archive the world, delete its volume, release the
 *  server, and for a deletion remove it); until then the server stays stopped and
 *  cannot be woken or claimed. Only the owner and admins are shown it. */
export interface RetireState {
  requested_at: string;
  delete: boolean;
}

/** MyServerView is the GET /me/servers row (wrapped under { servers: [...] }).
 *  Only this projection says whether the caller owns or may claim a server.
 *  The live fields come from the CRD best-effort; desiredState, autostartPolicy
 *  and playerCountUnknown are present on the caller's own rows only. */
export interface MyServerView {
  name: string;
  subdomain: string;
  /** Whether the caller owns it. */
  owned: boolean;
  /** Whether the caller may claim this server (unowned + linked + quota). */
  claimable: boolean;
  phase?: Phase;
  playersOnline: number;
  playersMax: number;
  displayName?: string;
  desiredState?: "Running" | "Stopped";
  autostartPolicy?: AutostartPolicy;
  /** True while the operator cannot read the player count; a stop may drop players. */
  playerCountUnknown?: boolean;
  /** How often the operator recreated the pod of this start after it timed out. */
  autoRestarts?: number;
  /** True for a Failed server no automatic retry will bring up. */
  startGaveUp?: boolean;
  /** A pending retirement; present on the caller's own rows only. */
  retiring?: RetireState;
}

/** ServerStatus is GET /servers/{name}/status (Go ServerInfo). It never carries
 *  `owned` or `claimable`; owner-tier gates read /me/servers via lib/ownership.
 *  A caller who does not own the server gets the public subset, so everything
 *  past the counts may be absent. */
export interface ServerStatus {
  startup?: { stage: "creating" | "scheduling" | "preparing" | "booting" | "failed"; reason?: string; message?: string; startedAt?: string; logsAvailable: boolean };
  nodeName?: string;
  name: string;
  subdomain: string;
  phase: Phase;
  ready: boolean;
  autostartPolicy?: AutostartPolicy;
  desiredState?: "Running" | "Stopped";
  endpointMode?: string;
  endpointAddress?: string;
  playersOnline: number;
  playersMax: number;
  displayName?: string;
  image?: string;
  /** The JVM heap derived from `memory`. */
  javaMemory?: string;
  /** The pod memory limit — the server's memory as an admin picks it (e.g. "4Gi"). */
  memory?: string;
  storageSize?: string;
  cpu?: string;
  /** Seconds empty before idle auto-stop; 0 when the server never idles out. */
  idleStopSeconds: number;
  /** True while the operator cannot read the player count; idle stop waits. */
  playerCountUnknown?: boolean;
  /** True when the CR is labelled forwarding=legacy: the proxy forwards this
   *  server's players BungeeCord-style (a 1.8 backend behind ViaVersion). */
  legacyForwarding?: boolean;
  /** How often the operator recreated the pod of this start after it timed out. */
  autoRestarts?: number;
  /** True for a Failed server no automatic retry will bring up; a Failed server
   *  without it is still in its restart backoff. */
  startGaveUp?: boolean;
  /** A platform system server the reaper never touches, so it cannot be given
   *  up or deleted. Owner and admin view only. */
  reaperExempt?: boolean;
  /** The server software of a runtime-image server; absent for a server image.
   *  Owner and admin view only. */
  software?: ServerSoftware;
  /** A pending retirement. Owner and admin view only. */
  retiring?: RetireState;
}

/** The kinds of server software a server can run from the runtime image. */
export type SoftwareType = "paper" | "purpur" | "fabric" | "quilt" | "forge" | "neoforge" | "vanilla" | "custom";

/** ServerSoftware is what a runtime-image server runs (Go ServerSoftware).
 *  forwarding is velocity when the server takes the proxy's player identity,
 *  none when players join it under offline-mode UUIDs. */
export interface ServerSoftware {
  type: SoftwareType;
  minecraftVersion?: string;
  /** The Paper/Purpur build or the loader version. */
  build?: string;
  java: 8 | 17 | 21 | 25;
  /** A custom server's jar, relative to the server directory. */
  jar?: string;
  forwarding: "velocity" | "none";
}

/** SoftwareRequest picks server software for a create or patch: build defaults
 *  to the recommended one, java to the runtime the version needs. */
export interface SoftwareRequest {
  type: SoftwareType;
  minecraftVersion?: string;
  build?: string;
  java?: 8 | 17 | 21 | 25;
  jar?: string;
}

export interface SoftwareVersion {
  id: string;
  /** False for pre-releases, release candidates and snapshots. */
  stable: boolean;
}

export interface SoftwareBuild {
  id: string;
  stable: boolean;
  /** The build preselected when none is picked. */
  recommended?: boolean;
}

/** SoftwareVersionInfo is GET /software/{type}/versions/{version}. */
export interface SoftwareVersionInfo {
  version: string;
  requiredJava: number;
  /** The runtime the version gets by default; absent when none is new enough. */
  java?: number;
  builds: SoftwareBuild[];
  forwarding: "velocity" | "none";
}

/** QuotaLimits are a quota's caps: an absent cap is unlimited, 0 grants none. */
export interface QuotaLimits {
  max_servers?: number;
  max_cpu_milli?: number;
  max_memory_mb?: number;
  max_storage_gb?: number;
}

/** ServerQuota is GET /me/server-quota: the quota in force for the caller and
 *  whether they can create a server under it. */
export interface ServerQuota {
  source: "" | "user" | "default";
  limits: QuotaLimits | null;
  usage: { servers: number; cpu_milli: number; memory_mb: number; storage_mb: number };
  canCreate: boolean;
  reason?: "not_linked" | "self_service_disabled" | "software_unavailable" | "quota_full";
}

/** SelfServiceQuotaSettings is the Owner's default quota (GET /settings/self-service). */
export interface SelfServiceQuotaSettings {
  enabled: boolean;
  max_servers: number | null;
  max_cpu_milli: number | null;
  max_memory_mb: number | null;
  max_storage_gb: number | null;
  revision: string;
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

/** AllowlistEntry is one row of GET /servers/{name}/allowlist: a player who joined
 *  the server, and so may wake it under autostartPolicy "allowlist" unless the owner
 *  took that away (can_wake false; the row stays so a rejoin cannot undo it).
 *  username is the live Felis account the UUID is linked to, absent when none. */
export interface AllowlistEntry {
  mc_uuid: string;
  username?: string;
  added_at: string;
  can_wake: boolean;
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
 *  ONLY source of WHO is online — MyServerView.playersOnline carries the count alone.
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
 *  It is a DISTINCT type from MyServerView, not a reuse: /fleet emits the raw CRD
 *  shape — `ready` and the `endpoint*` runtime fields, with playersOnline/playersMax
 *  required — whereas MyServerView is the /me/servers projection.
 *  Sharing one interface would blur which fields each face actually guarantees. */
export interface FleetServer extends ServerStatus {
  /** Owner's display identity (email, or username when the address is absent).
   *  Empty/absent for an unclaimed server or when the owner lookup failed;
   *  ownerUnknown tells the two apart. */
  owner?: string;
  /** The caller claimed this server, decided by account id on the server. */
  owned: boolean;
  /** Live, unclaimed and not a system service; false while ownership is unknown. */
  claimable: boolean;
  /** The owner lookup failed: an absent owner says nothing about the claim. */
  ownerUnknown?: boolean;
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
 *  list is created_at-descending, so the first row for a given server that is not
 *  `corrupt` is exactly the one a restore would recover (LatestBackup's WHERE
 *  mirrors this) — the UI must name that row, not a plausible proxy.
 *  `reason`/`status` cross an unvalidated JSON boundary; render unknown values
 *  tolerantly. */
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
  /** True once the archive failed a read-back (its sha256 or its gzip/tar did
   *  not check out). It stays listed so the loss is visible, and the API refuses
   *  to restore it (409 backup_corrupt). */
  corrupt?: boolean;
  /** When the reaper last read the archive back intact. Absent until the first
   *  read-back after it was written. */
  verified_at?: string;
  /** How many entries of the world were not plain files or directories
   *  (symlinks, sockets) and so are not in the archive. */
  skipped_entries?: number;
}

/** ServerJob is one row of GET /api/v1/servers/{name}/jobs — the observable
 *  outcome of an async backup/restore/export Job. The API only enqueues Jobs, so
 *  this projection is how a 202 that later failed becomes visible in the panel.
 *  An export_world or export_files Job holds the world until its download
 *  ends; an export_backup Job only reads the backup store. */
export interface ServerJob {
  name: string;
  kind: string; // "backup" | "restore" | "export_world" | "export_backup" | "export_files"
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
  /** A backup felis-api took on its own: the daily restore point of a world
   *  played since its last one, taken once the server stops. */
  scheduled?: boolean;
}

// ---- World export (internal/api/exports.go exportTicketView, exportStatusView) ----

/** ExportTicket is the 202 of POST /servers/{name}/backups/{id}/export and
 *  /servers/{name}/world/export: the one-time handle of a download being
 *  prepared. Only the viewer who asked can read or use it, and it opens the
 *  download once. `filename` is what the browser saves. */
export interface ExportTicket {
  ticket: string;
  state: "pending";
  filename: string;
}

/** ExportStatus is GET /exports/{ticket}: "pending" while the export Job is on
 *  its way, "ready" once felis-api holds the archive and waits (90 s) for the
 *  browser to fetch it, "failed" when the Job gave up, with its reason in
 *  message. A ticket already downloaded or past its time answers 410
 *  export_expired instead. */
export interface ExportStatus {
  state: "pending" | "ready" | "failed";
  message?: string;
}

// ---- Scheduled tasks (internal/api/schedules.go Schedule, scheduleInput) ----

export type ScheduleAction = "command" | "restart" | "stop" | "start" | "backup";
/** 0 runs once a day at minute_of_day; the rest divide a day, so the runs sit at
 *  the same clock times every day. A restart, stop, start or backup repeats at
 *  most every 60 minutes. */
export type ScheduleEveryMinutes = 0 | 15 | 30 | 60 | 120 | 180 | 240 | 360 | 480 | 720;
/** How long before a restart, stop or backup the players are told in game. */
export type ScheduleWarnMinutes = 0 | 1 | 5 | 10 | 15 | 30;
/** The step of a run in progress; empty when none is. */
export type ScheduleRunState = "" | "claimed" | "stopping" | "backing_up" | "starting";
/** How the last run ended; empty before the first and during a run. */
export type ScheduleResult = "" | "ok" | "skipped" | "failed" | "missed";

/** Schedule is one scheduled task of a server (GET /servers/{name}/schedules).
 *  It runs at minute_of_day on the weekdays (bit 0 Sunday … bit 6 Saturday), or
 *  with every_minutes set at every multiple of it since midnight on those days,
 *  in timezone. last_detail is the backend's English. */
export interface Schedule {
  id: number;
  server: string;
  label: string;
  action: ScheduleAction;
  /** The console command of a command task, without a slash; empty otherwise. */
  command: string;
  every_minutes: ScheduleEveryMinutes;
  minute_of_day: number;
  weekdays: number;
  timezone: string;
  warn_minutes: ScheduleWarnMinutes;
  enabled: boolean;
  /** Null while disabled. */
  next_run_at: string | null;
  run_state: ScheduleRunState;
  last_run_at: string | null;
  last_result: ScheduleResult;
  last_detail: string;
  created_by: string;
  created_at: string;
}

/** ScheduleInput is the body of a create (POST) or a save (PUT). The server trims
 *  the label, drops a command's leading slash, and refuses a command on any other
 *  action and a warning on a command or start. enabled defaults to true. */
export interface ScheduleInput {
  label?: string;
  action: ScheduleAction;
  command?: string;
  every_minutes?: ScheduleEveryMinutes;
  minute_of_day?: number;
  weekdays: number;
  timezone: string;
  warn_minutes?: ScheduleWarnMinutes;
  enabled?: boolean;
}

/** WhitelistImage is one row of GET /images (the create-form dropdown source). */
export interface WhitelistImage {
  image_ref: string;
  enabled: boolean;
  source: string;
  /** Set when the image came out of a panel build (build.Image BuildID). */
  build_id?: string;
  added_by: string;
  added_at: string;
}

/** CreateServerRequest is the §15 structured form — the ONLY create path. */
export interface CreateServerRequest {
  nodeName?: string;
  name: string;
  subdomain: string;
  displayName?: string;
  /** A whitelisted server image, or else software; never both. */
  image?: string;
  software?: SoftwareRequest;
  /** Required with software: the Minecraft EULA it runs under. */
  acceptEula?: boolean;
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

/** SelfCreateRequest is POST /me/servers: a server of the caller's own, always
 *  server software, within their quota. */
export interface SelfCreateRequest {
  name: string;
  subdomain: string;
  displayName?: string;
  software: SoftwareRequest;
  memory: string;
  cpu?: string;
  storage: string;
  autostartPolicy?: AutostartPolicy;
  acceptEula: boolean;
}

export interface MinecraftAuthSource {
  tag: string;
  lookup_available: boolean;
}

export interface AuthSourceConfig {
  tag: string;
  prefix: string;
  url: string;
  api_url: string;
  enabled: boolean;
}

/** MailSettings is the relay in force (GET /settings/smtp): the Owner's, felis.toml's, or none.
 *  The password never comes back; password_set says whether one is stored. */
export interface MailSettings {
  source: "panel" | "host" | "none";
  host: string;
  port: number;
  from: string;
  username: string;
  password_set: boolean;
  require_tls: boolean;
  max_per_hour: number;
  revision: string;
}

/** MailSettingsRequest is a relay to save or test; an absent password keeps the current one. */
export interface MailSettingsRequest {
  host: string;
  port: 25 | 465 | 587 | 2525;
  from: string;
  username: string;
  password?: string;
  require_tls: boolean;
  max_per_hour: number;
  revision: string;
}

/** RetiredAuthSource is a deleted source's tag with the endpoint it pointed at; the tag
 *  can be added again only with that endpoint. */
export interface RetiredAuthSource {
  tag: string;
  url: string;
}

export interface AuthSourcesSettings {
  sources: AuthSourceConfig[];
  retired: RetiredAuthSource[];
  revision: string;
  managed: boolean;
}

export interface MinecraftProfile {
  source: string;
  name: string;
  profile_uuid: string;
  mc_uuid: string;
  auth_source: string;
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
  request_id?: string;
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

/** FileUploadSession is where an upload sent in parts stands
 *  (internal/api/handlers_fileops.go fileSessionView): the next part starts at
 *  `received` and carries at most `part_max_bytes`. `parts` are the parts taken
 *  so far, in order, each with the SHA-256 (hex) it arrived with. */
export interface FileUploadSession {
  id: string;
  path: string;
  size: number;
  received: number;
  part_max_bytes: number;
  parts: { size: number; sha256: string }[];
}

/** FileOp is one background upload landing or extraction
 *  (handlers_fileops.go fileOpView). `done` and `total` are bytes, both 0 until
 *  the Job first reports; `files` and `bytes` are what an extraction wrote. */
export interface FileOp {
  id: string;
  op: "upload" | "unzip";
  path: string;
  state: "running" | "succeeded" | "failed";
  started_at: string;
  finished_at?: string;
  done: number;
  total: number;
  files?: number;
  bytes?: number;
  error?: FileOpError;
}

/** FileOpError is why an op failed (handlers_fileops.go fileOpError). On
 *  file_exists from an extraction, `conflicts` lists the first 200 files it
 *  would replace and `conflict_count` all of them; on volume_full, `need` and
 *  `avail` are bytes. */
export interface FileOpError {
  code: string;
  message: string;
  entry?: string;
  conflicts?: string[];
  conflict_count?: number;
  need?: number;
  avail?: number;
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
  /** sha256 of the build context the Job actually fetched. */
  context_digest?: string;
}

export type ScanSeverity = "CRITICAL" | "HIGH" | "MEDIUM" | "LOW" | "UNKNOWN";

/** ScanFinding mirrors build.ScanFinding — one vulnerability or leaked secret. */
export interface ScanFinding {
  id: string;
  kind: "vulnerability" | "secret";
  severity: ScanSeverity;
  package?: string;
  installed?: string;
  /** The first release that fixes it; absent when none exists. */
  fixed?: string;
  target: string;
  title?: string;
  blocking: boolean;
  /** The policy accepts this id, so it never blocks; absent when false. */
  accepted?: boolean;
}

/** ScanPolicy mirrors build.ScanPolicy ([registry] scan_fail_on / scan_fail_unfixed / scan_accept). */
export interface ScanPolicy {
  fail_on: ScanSeverity[];
  fail_unfixed: boolean;
  /** Finding ids accepted as known risks; absent when none. */
  accept?: string[];
}

/** ScanSummary mirrors build.ScanSummary — the verdict scan-gate reached. */
export interface ScanSummary {
  policy: ScanPolicy;
  blocked: boolean;
  packages: number;
  counts: Record<string, number>;
  blocking_counts: Record<string, number>;
  /** Blocking first, then most severe first; at most 100. */
  findings: ScanFinding[];
  omitted?: ("report" | "sbom")[];
}

/** BuildScan is GET /images/build/{id}/scan. */
export interface BuildScan {
  build_id: string;
  scanned_at: string;
  summary: ScanSummary;
  has_report: boolean;
  has_sbom: boolean;
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

/** ContextUploadProgress is where a chunked context upload stands: received is
 *  how many bytes the server holds (the next part starts there), part_max_bytes
 *  caps one part, max_context_bytes caps the whole context. */
export interface ContextUploadProgress {
  received: number;
  part_max_bytes: number;
  max_context_bytes: number;
}

/** SubmissionListParams picks one page of a submission list (server-side filter
 *  and paging; the scope is the endpoint, never a parameter). */
export interface SubmissionListParams {
  status?: SubmissionStatus;
  query?: string;
  limit?: number;
  offset?: number;
}

/** SubmissionPage is one page of a submission list: total counts the rows that
 *  match status and query, counts the scope's rows per status regardless. */
export interface SubmissionPage {
  submissions: Submission[];
  total: number;
  counts: Record<SubmissionStatus, number>;
}

export interface UpdateWindow {
  start: string | null;
  end: string | null;
}

// ---- Control-plane database backup (internal/api/handlers_dbbackup.go dbBackupView) ----

export type DBBackupLabel = "daily" | "pre-migrate" | "pre-restore" | "offsite" | "manual";

export interface DBBackupRecord {
  at: string;
  name: string;
  label: DBBackupLabel;
  size_bytes: number;
  felis_version?: string;
  schema_version?: number;
  dir: string;
  /** Why the bundle lacks the MinecraftServer objects, when it does: a restore from it brings back no servers. */
  servers_error?: string;
  /** When the newest daily bundle in dir was written, as of this record; absent when there was none. */
  daily_at?: string;
}

export interface DBBackupStatus {
  /** Null until the host has recorded its first backup. */
  last: DBBackupRecord | null;
  /** True when there is no record, no daily bundle, or the newest daily bundle is older than max_age_seconds. */
  stale: boolean;
  max_age_seconds: number;
}

// ---- Component version check (internal/api/handlers_updates.go updateReportView) ----

export type UpdateComponentState = "current" | "available" | "unknown" | "unreadable" | "pinned";

export interface UpdateComponent {
  name: string;
  /** Installed version; absent when unreadable. */
  current?: string;
  /** The newer stable release; present only when state is available. */
  latest?: string;
  state: UpdateComponentState;
  /** The `felis update --<selector>` flag that prints how to apply it. */
  selector?: string;
  note?: string;
  /** Why a version is missing (unknown / unreadable). */
  error?: string;
}

export interface UpdateStatusReport {
  checked_at: string;
  felis: string;
  components: UpdateComponent[];
}

export interface UpdateReport {
  /** Null until the host's felis-update-check.timer has recorded a check. */
  report: UpdateStatusReport | null;
  /** True when there is no record or it is older than max_age_seconds. */
  stale: boolean;
  max_age_seconds: number;
}

// ---- User admin types (internal/api/repo.go UserView, UserDetail, QuotaView, SessionView) ----

export interface UserView {
  id: string;
  username: string;
  /** Omitted for accounts created without one (bind-code / op-login only). */
  email?: string;
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
  deleted_at?: string;
  /** Omitted when the user has no linked Minecraft account. */
  linked_accounts?: LinkedAccount[];
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
  /** True when the user has a quota of their own; otherwise every cap is
   *  absent and the default quota (when on) applies to them. */
  custom: boolean;
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

// One registered passkey as GET /account/passkey/credentials lists it.
export interface PasskeyCredential {
  id: string;
  name: string;
  aaguid?: string;
  created_at: string;
  last_used_at?: string;
}

export interface SessionView {
  token_hash: string;
  created_at: string;
  expires_at: string;
  /** When the session last authenticated a request (recorded at most once a minute). */
  last_seen_at: string;
  /** The browser's User-Agent at sign-in; empty when none was sent. */
  user_agent: string;
  /** The address the sign-in came from; empty when unknown. */
  client_ip: string;
  revoked_at?: string;
  /** On the holder's own list only: the session this request came in on. */
  current?: boolean;
}

export interface ExecutionNode {
  name: string;
  role: string;
  ready: boolean;
  approved: boolean;
  addresses: string[];
  architecture: string;
  k3sVersion: string;
  /** The felis build the node last installed; absent for a node enrolled before it was recorded. */
  felisVersion?: string;
  /** A lost worker taken out of service for a failover: cordoned, no longer approved, tainted out of service. */
  fenced: boolean;
  /** When an offline node stopped reporting ready. */
  lostSince?: string;
}
export interface WorldMigration {
  id: string;
  server: string;
  state: string;
  stage: string;
  sourceNode: string;
  targetNode: string;
  sourcePVC: string;
  targetPVC: string;
  error?: string;
  switched: boolean;
  attempt: number;
  /** "failover" for a failover; absent for a migration. */
  kind?: "failover";
  /** The desired state a failover gives the server back once it has switched. */
  resume?: string;
}

export interface WakePolicySettings {
  maxRunningServers: number;
  wakeCooldownSeconds: number;
  revision: string;
  managed: boolean;
}

/** The watchdog heartbeat setting (GET/PUT/DELETE /settings/heartbeat): only the URL's
 *  scheme and host are shown. */
export interface HeartbeatSettings {
  set: boolean;
  display: string;
  revision: string;
}

/** The backup and audit retention in force (GET/PUT/DELETE /settings/archive-policy). */
export interface ArchivePolicy {
  manualKeep: number;
  manualRetentionDays: number;
  manualCooldownMinutes: number;
  /** 0 turns the scheduled restore points off. */
  scheduledEveryHours: number;
  scheduledKeep: number;
  scheduledRetentionDays: number;
  releasedRetentionDays: number;
  /** 0 for no cap. */
  maxStoreGiB: number;
  /** 0 keeps every audit row; otherwise at least 30. */
  auditRetentionDays: number;
}

export interface ArchivePolicySettings extends ArchivePolicy {
  source: "panel" | "host";
  revision: string;
}

/** The Owner's failover setting (GET/PUT /settings/failover). */
export interface FailoverPolicySettings {
  automatic: boolean;
  afterMinutes: number;
  /** 0 turns the replicas off; otherwise 15 to 1440. */
  replicaMinutes: number;
  revision: string;
  managed: boolean;
}

/** The backup a failover would restore. */
export interface FailoverBackup {
  id: string;
  createdAt: string;
  sizeBytes: number;
}

export interface FailoverServer {
  name: string;
  desiredState: string;
  backup: FailoverBackup | null;
  operation: WorldMigration | null;
}

/** What failing a lost worker over would do (GET /nodes/{node}/failover). */
export interface FailoverPlan {
  node: string;
  lost: boolean;
  fenced: boolean;
  lostSince: string | null;
  servers: FailoverServer[];
  targets: string[];
}

export interface FailoverSkip {
  name: string;
  reason: "no_backup" | "unknown_server" | "failed";
  error?: string;
}

export interface FailoverResult {
  operations: WorldMigration[];
  skipped: FailoverSkip[];
  fenceTask?: NodeControlTask;
  fenceError?: string;
}

export type NodeLifecycleAction = "enable" | "join" | "approve" | "upgrade" | "fence" | "unfence" | "enable-ha" | "join-controller";
/** Platform operations the panel runs on the controller host through node-control. */
export type HostTaskAction =
  | "db-backup" | "db-verify" | "db-delete" | "db-restore"
  | "offsite-sync" | "offsite-list" | "offsite-check-key" | "offsite-take-over" | "offsite-configure" | "offsite-disable" | "restore-host"
  | "backup-now" | "rotate-token" | "domain-check" | "domain-set" | "storage-configure" | "connection-configure"
  | "converge" | "doctor" | "support-bundle" | "host-settings" | "server-forwarding";
export interface NodeControlRequest {
  action: NodeLifecycleAction | HostTaskAction;
  name?: string;
  sshTarget?: string;
  externalIP?: string;
  peers?: string[];
  confirmMaintenance: boolean;
  /** The action's own inputs, kept with the task. */
  params?: Record<string, string>;
  /** Credentials the host needs; never stored with the task or returned. */
  secrets?: Record<string, string>;
}
export interface NodeControlTask {
  id: string;
  request: NodeControlRequest;
  actor: string;
  state: "running" | "succeeded" | "failed";
  stage: string;
  startedAt: string;
  finishedAt?: string;
  error?: string;
  log?: string;
  /** What the action reports as data, such as a health check's findings. */
  result?: Record<string, unknown>;
  /** A file the action produced, downloadable from the task. */
  artifact?: string;
}

export interface HostAlert {
  key: string;
  severity: "warning" | "critical";
  summary: string;
  summary_en: string;
  hint?: string;
  firstSeen: string;
  notified?: string;
}
export interface HostOverview {
  host: string;
  at: string;
  version: string;
  disks: { path: string; total: number; available: number }[];
  memory?: { total: number; available: number };
  units: { name: string; active: string; next?: string; last?: string }[];
  proxy: { address?: string; reachable?: boolean };
  alerts: HostAlert[];
  alertsError?: string;
  quietUntil?: string;
  databaseBackups: { name: string; label: string; size: number; created: string }[];
  databaseBackupsError?: string;
  offsite: {
    configured: boolean;
    endpoint?: string;
    bucket?: string;
    region?: string;
    prefix?: string;
    dbKeep?: number;
    keyKept: boolean;
    status?: Record<string, unknown>;
    error?: string;
  };
  settings: { dbBackupTime: string; dbBackupKeep: number; velocityXmx: string };
  connection: { mode: "local" | "reverse-proxy" | "cloudflare"; panelHost: string; adminHost: string };
  storage: { mode: "local" | "s3"; endpoint?: string; bucket?: string; region?: string };
  rootDomain: string;
}
/** The off-site run as the host records it (internal/offsite Status). */
export interface OffsiteStatus {
  last_attempt?: string;
  last_success?: string;
  last_error?: string;
  bucket?: string;
  key_id?: string;
  key_mismatch?: boolean;
  standby?: boolean;
  displaced?: boolean;
  writer?: { host_id: string; host: string; at: string };
}
/** A doctor run as the panel shows it. */
export interface DoctorFinding {
  key: string;
  severity: "warning" | "critical";
  summary: string;
  summary_en: string;
  hint?: string;
}
export interface DoctorView {
  host: string;
  at: string;
  source: string;
  areas: { key: string; title: string; state: "ok" | "problem" | "skipped"; skipped?: string; findings?: DoctorFinding[] }[];
  notes?: string[];
  critical: number;
  warning: number;
}

export interface EntryPolicySettings {
  mode: "lobby" | "direct" | "domain";
  defaultServer: string;
  requireAccountLink: boolean;
  offlineAction: "wake" | "fallback" | "disconnect";
  waitingSpace: "login" | "lobby";
  fallbackServer: string;
  revision: string;
}
