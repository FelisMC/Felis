import type { ApiError, Identity } from "./types";

// Only /me 401 signs out; transient failures retain user-side access without elevated roles.

export interface AuthState {
  identity: Identity | null;
  loading: boolean;
  isAdmin: boolean;
  unauthenticated: boolean;
  teamModeBlocked: boolean;
  identityError: unknown;
}

export function isUnauthorized(error: unknown): boolean {
  return (
    typeof error === "object" &&
    error !== null &&
    (error as Partial<ApiError>).status === 401
  );
}

export function isTeamModeBlocked(error: unknown): boolean {
  if (typeof error !== "object" || error === null) return false;
  const err = error as Partial<ApiError>;
  return err.status === 403 && err.code === "team_mode_staff_only";
}

// Malformed identity fields cannot grant administrator access.
export function deriveAuth(
  identity: Identity | null,
  error: unknown,
  loading: boolean,
): AuthState {
  const blocked = !loading && identity === null && isTeamModeBlocked(error);
  const unauthenticated = !loading && identity === null && (isUnauthorized(error) || blocked);
  return {
    identity,
    loading,
    isAdmin: identity?.is_admin === true,
    unauthenticated,
    teamModeBlocked: blocked,
    identityError: !loading && identity === null && error != null && !unauthenticated ? error : null,
  };
}

// Accept only same-origin absolute paths; reject protocol-relative, backslash, and control-character redirects.
export function loginReturnPath(next: string | null): string {
  if (!next || !next.startsWith("/") || next.startsWith("//")) return "/";
  if (/[\\\u0000-\u001f]/.test(next)) return "/";
  if (/^\/(login|setup)(\/|\?|$)/.test(next)) return "/";
  return next;
}
