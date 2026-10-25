// Mirror of the felis-api external-face JSON. Kept narrow: only the fields the panel renders, so the UI never depends on internal shapes.

// JSON is unvalidated at runtime; phase maps still need an unknown-value fallback.
export type Phase =
  | "Stopped"
  | "Starting"
  | "Running"
  | "Stopping"
  | "Failed"
  | "Unknown";

export type AutostartPolicy = "ownerOnly" | "public" | "allowlist";

export interface ServerPowerResult {
  name: string;
  desiredState: "Running" | "Stopped";
}

export interface StartupEnvVar {
  name: string;
  value: string;
}

export interface ServerStartupSettings {
  java_flags: string[];
  args: string[];
  env: StartupEnvVar[];
  stop_commands: string[];
}

export interface StartupSettingsPatch {
  java_flags?: string[] | null;
  args?: (string | null)[] | null;
  env?: { name: string; value?: string | null }[] | null;
  stop_commands?: string[] | null;
}

// Pending retirement prevents waking or claiming until the reaper completes it.
export interface RetireState {
  requested_at: string;
  delete: boolean;
}

export interface ServerDiskUsage {
  usedBytes: number;
  limitBytes: number;
  sampledAt: string;
  stale: boolean;
}

export interface ServerExtraPortRequest {
  protocol: "TCP" | "UDP";
  port: number;
}

export interface ServerExtraPort extends ServerExtraPortRequest {
  nodePort?: number;
  address?: string;
}

// Only /me/servers reports ownership and claimability; private lifecycle fields appear on owned rows.
export interface MyServerView {
  extraPorts?: ServerExtraPort[];
  extraPortsPending?: boolean;
  disk?: ServerDiskUsage;
  diskFull?: boolean;
  cpu?: string;
  memory?: string;
  cpuMilli?: number;
  memoryBytes?: number;
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

// Public status may omit private fields; ownership gates must use /me/servers.
export interface ServerStatus {
  extraPorts?: ServerExtraPort[];
  extraPortsPending?: boolean;
  disk?: ServerDiskUsage;
  diskFull?: boolean;
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
  cpuMilli?: number;
  memoryBytes?: number;
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
  /** Whether a server that was running starts again after its host restarts. */
  resumeAfterHostRestart: boolean;
  /** Whether the server is restarted after its process exits or stops responding. */
  restartOnCrash: boolean;
  /** Crashes in a row restarted before the server stops (1–20); kept while restarts are off. */
  maxCrashRestarts: number;
  notify: ServerNotify;
  /** Crashes in a row, the latest within the last hour. */
  recentCrashes?: number;
  lastCrash?: CrashInfo;
  /** Why the platform stopped the server on its own; cleared once it starts. */
  autoStop?: AutoStopInfo;
  /** Stopped after crashing: only its owner or an admin starts it. */
  crashStopped?: boolean;
  /** The latest lifecycle events, oldest first. */
  events?: ServerEvent[];
}

export interface UsagePoint {
  at: string;
  cpuMilli: number;
  memoryBytes: number;
}

export interface ServerUsage {
  current: UsagePoint | null;
  history: UsagePoint[];
}

export interface ConsoleHistoryPage {
  content: string;
  content_sha256: string;
  next_cursor?: string;
  has_more: boolean;
  byte_limited: boolean;
}

export interface ConsoleCommandResult {
  name: string;
  output: string;
  transport: "rcon" | "stdin";
}

/** The lifecycle events mailed to the server's owner. */
export interface ServerNotify {
  started: boolean;
  stopped: boolean;
  /** Crashes, failed starts and host restarts. */
  problems: boolean;
}

export interface CrashInfo {
  at: string;
  /** The container termination reason, such as OOMKilled. */
  reason?: string;
  exitCode: number;
}

export type AutoStopReason = "Idle" | "Crashed" | "CrashLimit" | "HostRestarted" | "DiskFull";

export interface AutoStopInfo {
  reason: AutoStopReason;
  at: string;
}

export type ServerEventType = "Started" | "Stopped" | "Crashed" | "HostRestarted" | "StartFailed" | "DiskWarning" | "DiskFull";

export interface ServerEvent {
  seq: number;
  type: ServerEventType;
  at: string;
  reason?: string;
  exitCode?: number;
  crashes?: number;
  /** The restart limit in force; 0 when restarts were off. */
  limit?: number;
  /** The platform stopped the server because of this event. */
  stopping?: boolean;
}

/** The kinds of server software a server can run from the runtime image. */
export type SoftwareType = "paper" | "purpur" | "fabric" | "quilt" | "forge" | "neoforge" | "vanilla" | "custom";

// forwarding=none uses offline-mode UUIDs; velocity accepts the proxy's identity.
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

// Parsed player names are best-effort; raw RCON output remains authoritative.
export interface WhitelistResult {
  name: string;
  players: string[];
  output: string;
}

// Parse bans at " was banned by ", since reasons may contain colons.
export interface BanlistResult {
  name: string;
  players: string[];
  output: string;
}

// Retain can_wake=false rows so a later join cannot restore revoked wake access.
export interface AllowlistEntry {
  mc_uuid: string;
  username?: string;
  added_at: string;
  can_wake: boolean;
}

export interface AccessResult {
  name: string;
  action: string;
  player: string;
  output: string;
}

// Parsed player lists may be empty on custom servers; retain raw RCON output.
// online can therefore be greater than players.length: show the count, and fall back to output for names.
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

// Fleet uses raw CRD fields with required player counts, unlike the ownership projection.
export interface FleetServer extends ServerStatus {
  // ownerUnknown distinguishes failed owner lookup from an unclaimed server.
  owner?: string;
  /** The caller claimed this server, decided by account id on the server. */
  owned: boolean;
  /** Live, unclaimed and not a system service; false while ownership is unknown. */
  claimable: boolean;
  /** The owner lookup failed: an absent owner says nothing about the claim. */
  ownerUnknown?: boolean;
  // System-server rows are read-only because per-server routes reject reserved names.
  system?: boolean;
}

// Restore by eligible backup identity; tolerate unknown reason/status values from unvalidated JSON.
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
  // Corrupt archives remain visible but restoration rejects with backup_corrupt.
  corrupt?: boolean;
  /** When the reaper last read the archive back intact. Absent until the first
   *  read-back after it was written. */
  verified_at?: string;
  /** How many entries of the world were not plain files or directories
   *  (symlinks, sockets) and so are not in the archive. */
  skipped_entries?: number;
}

// World/files exports hold the world; backup exports hold only archive storage.
export interface ServerJob {
  name: string;
  kind: string; // "backup" | "restore" | "export_world" | "export_backup" | "export_files"
  state: string; // "running" | "succeeded" | "failed"
  message?: string;
  started_at?: string;
  finished_at?: string;
  // Safety snapshots hold pending until the chained restore starts or is abandoned.
  then_restore?: string;
  then_restore_reason?: string;
  restore_backup_id?: string;
  /** A backup felis-api took on its own: the daily restore point of a world
   *  played since its last one, taken once the server stops. */
  scheduled?: boolean;
}


// Export tickets are single-use and bound to the requesting viewer.
export interface ExportTicket {
  ticket: string;
  state: "pending";
  filename: string;
}

// Ready exports expire after 90 seconds; consumed or expired tickets return 410.
export interface ExportStatus {
  state: "pending" | "ready" | "failed";
  message?: string;
}


export type ScheduleAction = "command" | "restart" | "stop" | "start" | "backup";
// Zero means daily; maintenance actions repeat at most hourly.
export type ScheduleEveryMinutes = 0 | 15 | 30 | 60 | 120 | 180 | 240 | 360 | 480 | 720;
/** How long before a restart, stop or backup the players are told in game. */
export type ScheduleWarnMinutes = 0 | 1 | 5 | 10 | 15 | 30;
/** The step of a run in progress; empty when none is. */
export type ScheduleRunState = "" | "claimed" | "stopping" | "backing_up" | "starting";
/** How the last run ended; empty before the first and during a run. */
export type ScheduleResult = "" | "ok" | "skipped" | "failed" | "missed";

// Weekday bits run Sunday=0 through Saturday=6; interval multiples are measured from local midnight.
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

// Commands strip a leading slash; enabled defaults to true.
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

/** CreateServerRequest is the structured create form, the only create path. */
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

// The API's generic instructions are omitted because the panel provides localized copy.
export interface LinkStatus {
  linked: boolean;
}

// The linked Minecraft UUID is returned only by code verification.
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

// Keep snake_case JSON keys aligned with GET /me.
// Use server-computed is_admin for authorization display; role is a label only.
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

// received is the next part offset; part_max_bytes bounds each part.
export interface FileUploadSession {
  id: string;
  path: string;
  size: number;
  received: number;
  part_max_bytes: number;
  parts: { size: number; sha256: string }[];
}

export interface ExtensionMetadata {
  id: string;
  name: string;
  version?: string;
  authors: string[];
  loaders: string[];
}

export interface ServerExtension {
  path: string;
  filename: string;
  size: number;
  enabled: boolean;
  kind: "plugin" | "mod";
  metadata_status: "ok" | "unavailable";
  metadata: ExtensionMetadata[];
  config_path?: string;
}

export interface ServerExtensionsListing {
  entries: ServerExtension[];
  truncated: boolean;
}

export interface PaperGlobalValues {
  "chunk-loading-advanced.auto-config-send-distance"?: boolean;
  "chunk-loading-advanced.player-max-concurrent-chunk-generates"?: number;
  "chunk-loading-advanced.player-max-concurrent-chunk-loads"?: number;
  "chunk-loading-basic.player-max-chunk-generate-rate"?: number;
  "chunk-loading-basic.player-max-chunk-load-rate"?: number;
  "chunk-loading-basic.player-max-chunk-send-rate"?: number;
  "collisions.enable-player-collisions"?: boolean;
  "collisions.send-full-pos-for-hard-colliding-entities"?: boolean;
  "commands.suggest-player-names-when-null-tab-completions"?: boolean;
  "console.enable-brigadier-completions"?: boolean;
  "console.enable-brigadier-highlighting"?: boolean;
  "messages.use-display-name-in-quit-message"?: boolean;
  "messages.no-permission"?: string;
  "messages.kick.connection-throttle"?: string;
  "player-auto-save.max-per-tick"?: number;
  "player-auto-save.rate"?: number;
  "scoreboards.save-empty-scoreboard-teams"?: boolean;
  "scoreboards.track-plugin-scoreboards"?: boolean;
  "watchdog.early-warning-delay"?: number;
  "watchdog.early-warning-every"?: number;
}

export interface PaperGlobalConfig {
  values: PaperGlobalValues;
  sha256: string;
}

export interface PaperGlobalPatch {
  values: PaperGlobalValues;
  expect_sha256: string;
}

// done/total are progress bytes; files/bytes report extracted output.
export interface FileOp {
  id: string;
  op: "upload" | "unzip" | "copy" | "move" | "zip";
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

// Extraction conflicts list at most 200 names; need/avail are bytes on volume_full.
export interface FileOpError {
  code: string;
  message: string;
  entry?: string;
  conflicts?: string[];
  recovery_paths?: string[];
  conflict_count?: number;
  need?: number;
  avail?: number;
}

/** Build mirrors an image_builds row. */
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
  /** sha256 of the build context the Job fetched. */
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

/** Submission mirrors an image_submissions row. */
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

// received is the next offset; part_max_bytes and max_context_bytes bound uploads.
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
  /** A lost worker taken out of service for a failover: cordoned, unapproved, and tainted out of service. */
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
