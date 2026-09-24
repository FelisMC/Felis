import type { ServerInfo } from "./types";

// Owner-tier pages (console extras, players, files, backups, LuckPerms) decide
// who may act from GET /me/servers: the status projection never carries
// `owned`, so reading it there hides owner tools from every non-admin owner.

/** canManage reports whether the caller may use a server's owner-tier tools. */
export function canManage(
  isAdmin: boolean,
  mine: readonly ServerInfo[] | null | undefined,
  name: string,
): boolean {
  return isAdmin || (mine ?? []).some((s) => s.name === name && s.owned === true);
}

/** ownershipPending is true while the answer is still unknown: the tier is
 *  loading, or a non-admin's /me/servers read has neither landed nor failed. */
export function ownershipPending(
  tierLoading: boolean,
  isAdmin: boolean,
  mine: readonly ServerInfo[] | null | undefined,
  mineError: unknown,
): boolean {
  return tierLoading || (!isAdmin && mine == null && !mineError);
}
