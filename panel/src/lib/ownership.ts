import type { AccessRole, MyServerView, ServerStatus } from "./types";

export type EffectiveAccessRole = AccessRole | "none";

/** Resolves the caller's effective role on a server from status first, then /me/servers. */
export function resolveServerAccessRole(
  isAdmin: boolean,
  status: Pick<ServerStatus, "accessRole"> | null | undefined,
  mine: readonly MyServerView[] | null | undefined,
  name: string,
): EffectiveAccessRole {
  if (status?.accessRole) return status.accessRole;
  const entry = (mine ?? []).find((s) => s.name === name);
  if (entry?.accessRole) return entry.accessRole;
  if (entry?.owned) return "owner";
  return isAdmin ? "admin" : "none";
}

export function canOperateRole(role: EffectiveAccessRole): boolean {
  return role === "owner" || role === "admin" || role === "manager" || role === "operator";
}

export function canManageRole(role: EffectiveAccessRole): boolean {
  return role === "owner" || role === "admin" || role === "manager";
}

export function isReadonlyRole(role: EffectiveAccessRole): boolean {
  return role === "readonly";
}

export function canDeleteServer(isOwner: boolean, isAdmin: boolean, isProtected?: boolean): boolean {
  return isOwner || (isAdmin && !isProtected);
}

export function canManage(
  isAdmin: boolean,
  mine: readonly MyServerView[] | null | undefined,
  name: string,
): boolean {
  if (isAdmin) return true;
  return (mine ?? []).some((s) => s.name === name && (s.owned === true || s.accessRole === "owner" || s.accessRole === "manager"));
}

/** Reports true while tier or non-admin server ownership has not yet resolved. */
export function ownershipPending(
  tierLoading: boolean,
  isAdmin: boolean,
  mine: readonly MyServerView[] | null | undefined,
  mineError: unknown,
): boolean {
  return tierLoading || (!isAdmin && mine == null && !mineError);
}
