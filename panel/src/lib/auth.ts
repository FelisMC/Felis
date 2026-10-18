import type { ApiError, Identity } from "./types";

// Only /me 401 signs out; transient failures retain user-side access without elevated roles.

/** AuthState is the routing-relevant projection of a single /me outcome. */
export interface AuthState {
  /** The resolved identity, or null while loading / on any /me failure. */
  identity: Identity | null;
  /** True only while the initial /me request is in flight. */
  loading: boolean;
  /** Server-computed admin flag; false unless a loaded identity says is_admin. */
  isAdmin: boolean;
  /** True only when /me returned 401 (no or expired session, route to /login). A transient or 5xx failure leaves this false so the app keeps rendering. */
  unauthenticated: boolean;
  // Unknown identity retains the retryable error instead of treating the user as unauthorized.
  identityError: unknown;
}

export function isUnauthorized(error: unknown): boolean {
  return (
    typeof error === "object" &&
    error !== null &&
    (error as Partial<ApiError>).status === 401
  );
}

// Malformed identity fields cannot grant administrator access.
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
    identityError: !loading && identity === null && error != null && !isUnauthorized(error) ? error : null,
  };
}

// Accept only same-origin absolute paths; reject protocol-relative, backslash, and control-character redirects.
export function loginReturnPath(next: string | null): string {
  if (!next || !next.startsWith("/") || next.startsWith("//")) return "/";
  if (/[\\\u0000-\u001f]/.test(next)) return "/";
  if (/^\/(login|setup)(\/|\?|$)/.test(next)) return "/";
  return next;
}
