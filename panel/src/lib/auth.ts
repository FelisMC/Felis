import type { ApiError, Identity } from "./types";

// Pure auth-state derivation, kept out of tier.tsx so it can be pinned without a
// React renderer (mirrors lib/nav.ts). The whole session gate turns on one
// distinction the rest of the app routes on: a /me that returns 401 means "there
// is genuinely no session — show the login page", whereas ANY OTHER /me failure
// (network, 5xx, timeout) must NOT log the user out. The latter preserves the
// graded-Zero-Trust contract from tier.tsx rule 2: a principal whose /me momentarily
// fails still gets the User-Side app rather than being bounced to a login form they
// may have no way to satisfy (their real credential is the upstream Access proxy).

/** AuthState is the routing-relevant projection of a single /me outcome. */
export interface AuthState {
  /** The resolved identity, or null while loading / on any /me failure. */
  identity: Identity | null;
  /** True only while the initial /me request is in flight. */
  loading: boolean;
  /** Server-computed admin flag; false unless a loaded identity says is_admin. */
  isAdmin: boolean;
  /** True ONLY when /me returned 401 — no/expired session, route to /login. A
   *  transient or 5xx failure leaves this false so the app keeps rendering. */
  unauthenticated: boolean;
}

/** isUnauthorized reports whether a caught error is the request() 401 envelope —
 *  the one failure mode that means "no session" rather than "session unknown".
 *  Anything that is not specifically a 401 (network errors, 5xx, a thrown
 *  non-ApiError) is deliberately treated as NOT unauthenticated. */
export function isUnauthorized(error: unknown): boolean {
  return (
    typeof error === "object" &&
    error !== null &&
    (error as Partial<ApiError>).status === 401
  );
}

/** deriveAuth folds one /me outcome (identity OR error, plus the in-flight flag)
 *  into the state the router reads. Every boolean is computed with `=== true` / an
 *  explicit 401 check so an absent or malformed field fails to the safe side:
 *  non-admin, still-authenticated. */
export function deriveAuth(
  identity: Identity | null,
  error: unknown,
  loading: boolean,
): AuthState {
  return {
    identity,
    loading,
    isAdmin: identity?.is_admin === true,
    unauthenticated: !loading && identity === null && isUnauthorized(error),
  };
}
