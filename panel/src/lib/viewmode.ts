import { visibleSections, type NavSection } from "./nav";

// Derive view modes from navigation IDs and rank them explicitly; backend roles still govern access.

/** ViewMode is the home a principal is currently viewing. It is exactly the set of
 *  nav section ids, so the switcher and the sidebar share one vocabulary. */
export type ViewMode = NavSection["id"];

// Rank orders view visibility; adding a view requires an explicit rank.
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

// Non-admins receive only user mode, including while identity is unknown.
export function availableViewModes(isAdmin: boolean, isOwner: boolean): ViewMode[] {
  if (isOwner) return [...VIEW_MODES];
  if (isAdmin) return ["user", "admin"];
  return ["user"];
}

// Recheck requested modes against the live admin flag, including after demotion.
export function effectiveViewMode(
  requested: ViewMode | null | undefined,
  isAdmin: boolean,
  isOwner: boolean,
): ViewMode {
  const allowed = availableViewModes(isAdmin, isOwner);
  return requested != null && allowed.includes(requested) ? requested : "user";
}

// Shape validation alone grants no access; compose with effectiveViewMode.
export function parseViewMode(raw: unknown): ViewMode | null {
  return typeof raw === "string" && (VIEW_MODES as string[]).includes(raw)
    ? (raw as ViewMode)
    : null;
}

// Revalidate persisted modes against the current administrator flag.
export function restoreViewMode(raw: unknown, isAdmin: boolean, isOwner: boolean): ViewMode {
  return effectiveViewMode(parseViewMode(raw), isAdmin, isOwner);
}

// View selection can narrow navigation but cannot exceed the live role's visible sections.
export function sectionsForView(view: ViewMode, isAdmin: boolean, isOwner: boolean): NavSection[] {
  const ceiling = VIEW_RANK[effectiveViewMode(view, isAdmin, isOwner)];
  return visibleSections(isAdmin, isOwner).filter((s) => VIEW_RANK[s.id] <= ceiling);
}
