import { visibleSections, type NavSection } from "./nav";

// The avatar role-switcher model, kept as pure logic so the "which home is this
// principal actually in, and what may they switch into" decision is testable
// without rendering React — the same discipline as nav.ts (visibleSections) and
// auth.ts (deriveAuth). DESIGN-WEB-3SIDES / the identity model surface three homes
// by hostname tier: the User-Side home (mc.<root_domain>), the Admin-Side home
// (console.<root_domain>) and the SysAdmin-Side home (op.console.<root_domain>).
// The switcher in the top-right avatar lets a principal move between the homes
// they are entitled to.
//
// ViewMode is derived from NavSection["id"] on purpose: the three switchable homes
// ARE the three nav sections, so the two can never drift — add a section and the
// VIEW_RANK Record below stops compiling until the new home is given a rank.
//
// One invariant is load-bearing and security-relevant; the rest is UX focus:
//   * Fail-closed (security): a non-admin can NEVER end up in an Admin- or
//     SysAdmin-Side view, no matter what is requested, persisted, or tampered with.
//     Admin-ness is the single fail-closed `is_admin === true` flag (tier.tsx); the
//     switcher only ever narrows what an entitled principal sees, never widens it.
//   * Ceiling (UX only): the chosen view is a *ceiling* on which sections are
//     foregrounded. Hiding a section is convenience, NOT access control — every
//     /admin and /ops data call is independently 403-gated server-side (nav.ts).
//     So a wrong ceiling can declutter or clutter the sidebar, but can never grant
//     access; only the fail-closed rule above carries security weight.

/** ViewMode is the home a principal is currently viewing. It is exactly the set of
 *  nav section ids, so the switcher and the sidebar share one vocabulary. */
export type ViewMode = NavSection["id"];

// VIEW_RANK is the single source of truth for how much each home reveals, lowest
// first. It doubles as the section-visibility ceiling: a section is foregrounded
// in a view iff its own rank is at or below the view's rank. Typed as a total
// Record<ViewMode, ...> so a newly added home fails to compile until ranked —
// there is no silent "unranked → 0" default that could hide or leak a section.
const VIEW_RANK: Record<ViewMode, number> = {
  user: 0,
  admin: 1,
  owner: 2,
};

/** VIEW_MODES lists every home, ordered by how much it reveals (User → Ops). It is
 *  derived from VIEW_RANK so it can never fall out of sync with the rank table. */
export const VIEW_MODES: ViewMode[] = (Object.keys(VIEW_RANK) as ViewMode[]).sort(
  (a, b) => VIEW_RANK[a] - VIEW_RANK[b],
);

/**
 * availableViewModes returns the homes a principal may switch INTO, given the
 * fail-closed admin flag. A non-admin (or the `false` used while /me loads or after
 * it errors) gets exactly `["user"]`; an admin gets every home. This is the list the
 * avatar menu renders, and the allow-list effectiveViewMode resolves against.
 */
export function availableViewModes(isAdmin: boolean, isOwner: boolean): ViewMode[] {
  if (isOwner) return [...VIEW_MODES];
  if (isAdmin) return ["user", "admin"];
  return ["user"];
}

/**
 * effectiveViewMode resolves a *requested* view (a menu click, a persisted choice,
 * or null while nothing is chosen) against the LIVE admin flag, failing closed. It
 * is the single authority for which home a principal is actually in, and it never
 * trusts the request over the flag: the request is honoured only if it is in the
 * principal's currently-available set, so a non-admin — or an admin who has just
 * been demoted — always collapses to "user". An absent/null request also lands on
 * "user", the safe default.
 */
export function effectiveViewMode(
  requested: ViewMode | null | undefined,
  isAdmin: boolean,
  isOwner: boolean,
): ViewMode {
  const allowed = availableViewModes(isAdmin, isOwner);
  return requested != null && allowed.includes(requested) ? requested : "user";
}

/**
 * parseViewMode narrows an untrusted value (a string read back from localStorage, a
 * query param, anything) to a ViewMode, or null if it is not one of the known homes.
 * It performs NO gating — it validates shape only — so it must always be composed
 * with effectiveViewMode before the result is trusted (see restoreViewMode).
 */
export function parseViewMode(raw: unknown): ViewMode | null {
  return typeof raw === "string" && (VIEW_MODES as string[]).includes(raw)
    ? (raw as ViewMode)
    : null;
}

/**
 * restoreViewMode is the load-bearing restore path for a persisted view choice. It
 * takes whatever was stored (a localStorage string, or any untrusted value) plus the
 * LIVE admin flag, and resolves the home the principal actually gets — re-gating
 * every time. A stored "ops"/"admin" is honoured only while `isAdmin` is true right
 * now; a non-admin reading a stale or hand-edited "ops" out of their own storage
 * collapses to "user". The persisted value is never trusted over the live flag,
 * which is the one escalation vector a client-side persisted persona could open and
 * is closed here. Shape validation (parseViewMode) runs first so a malformed value
 * cannot slip past as a truthy non-ViewMode.
 */
export function restoreViewMode(raw: unknown, isAdmin: boolean, isOwner: boolean): ViewMode {
  return effectiveViewMode(parseViewMode(raw), isAdmin, isOwner);
}

/**
 * sectionsForView returns the nav sections foregrounded for a given requested view,
 * composed on top of visibleSections so it is fail-closed twice over: visibleSections
 * first drops every admin-gated section for a non-admin, then the view ceiling drops
 * any section ranked above the resolved view. The result is therefore always a subset
 * of what the principal's is_admin flag already permits — the switcher can only ever
 * narrow the sidebar, never widen it past `visibleSections(isAdmin)`.
 */
export function sectionsForView(view: ViewMode, isAdmin: boolean, isOwner: boolean): NavSection[] {
  const ceiling = VIEW_RANK[effectiveViewMode(view, isAdmin, isOwner)];
  return visibleSections(isAdmin, isOwner).filter((s) => VIEW_RANK[s.id] <= ceiling);
}
