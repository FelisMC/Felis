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
