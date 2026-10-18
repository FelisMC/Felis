import type { MyServerView } from "./types";

// Read ownership from /me/servers; public status never includes owned.

/** canManage reports whether the caller may use a server's owner-tier tools. */
export function canManage(
  isAdmin: boolean,
  mine: readonly MyServerView[] | null | undefined,
  name: string,
): boolean {
  return isAdmin || (mine ?? []).some((s) => s.name === name && s.owned === true);
}

/** ownershipPending is true while the answer is still unknown: the tier is
 *  loading, or a non-admin's /me/servers read has neither landed nor failed. */
export function ownershipPending(
  tierLoading: boolean,
  isAdmin: boolean,
  mine: readonly MyServerView[] | null | undefined,
  mineError: unknown,
): boolean {
  return tierLoading || (!isAdmin && mine == null && !mineError);
}
