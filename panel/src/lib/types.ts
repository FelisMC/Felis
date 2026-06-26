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
}
