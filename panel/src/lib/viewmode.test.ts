import { describe, it, expect } from "vitest";
import {
  VIEW_MODES,
  availableViewModes,
  effectiveViewMode,
  parseViewMode,
  restoreViewMode,
  sectionsForView,
  type ViewMode,
} from "./viewmode";
import { visibleSections } from "./nav";

// The role-switcher decides which home a principal is in and which they may switch
// into. One rule carries security weight and the rest is UX focus, so the cases
// below are split accordingly: the bulk pin the fail-closed rule from every angle a
// view can be chosen — a fresh request, a restored localStorage value, a tampered
// value — because the single thing that must never happen is a non-admin (or a
// demoted admin) landing in an Admin- or SysAdmin-Side view. The ceiling/section
// cases pin the navigational focus, which can declutter but never escalate.

describe("availableViewModes", () => {
  it("offers a non-admin exactly the User-Side home", () => {
    expect(availableViewModes(false)).toEqual(["user"]);
  });

  it("treats the fail-closed default (false) exactly like a non-admin", () => {
    // TierProvider passes `false` while /me loads or after it rejects; the switcher
    // must offer nothing but the user home in that window.
    expect(availableViewModes(false)).toEqual(["user"]);
  });

  it("offers an admin every home, ordered least- to most-revealing", () => {
    expect(availableViewModes(true)).toEqual(["user", "admin"]);
  });

  it("returns a fresh array so a caller cannot mutate the canonical list", () => {
    const a = availableViewModes(true);
    a.push("user");
    expect(availableViewModes(true)).toEqual(["user", "admin"]);
  });
});

describe("VIEW_MODES", () => {
  it("is the two homes ordered by how much they reveal", () => {
    expect(VIEW_MODES).toEqual(["user", "admin"]);
  });
});

describe("effectiveViewMode (fail-closed resolution)", () => {
  it("collapses any admin-level request from a non-admin to user", () => {
    expect(effectiveViewMode("admin", false)).toBe("user");
  });

  it("honours an admin's request for any home they are entitled to", () => {
    expect(effectiveViewMode("user", true)).toBe("user");
    expect(effectiveViewMode("admin", true)).toBe("admin");
  });

  it("defaults a null/undefined request to the user home for either tier", () => {
    expect(effectiveViewMode(null, true)).toBe("user");
    expect(effectiveViewMode(undefined, true)).toBe("user");
    expect(effectiveViewMode(null, false)).toBe("user");
  });

  it("collapses a value outside the known homes to user, even for an admin", () => {
    // Defends the runtime boundary: a value cast past the type system (a stale enum,
    // a hand-edited store) is not in the allow-list, so it fails to the safe side.
    expect(effectiveViewMode("root" as unknown as ViewMode, true)).toBe("user");
    expect(effectiveViewMode("" as unknown as ViewMode, true)).toBe("user");
  });
});

describe("parseViewMode (shape only, no gating)", () => {
  it("accepts each known home verbatim", () => {
    expect(parseViewMode("user")).toBe("user");
    expect(parseViewMode("admin")).toBe("admin");
  });

  it("rejects anything that is not a known home, returning null", () => {
    expect(parseViewMode("ops")).toBeNull();
    expect(parseViewMode("operator")).toBeNull();
    expect(parseViewMode("Admin")).toBeNull(); // case-sensitive on purpose
    expect(parseViewMode("")).toBeNull();
    expect(parseViewMode(null)).toBeNull();
    expect(parseViewMode(undefined)).toBeNull();
    expect(parseViewMode(2)).toBeNull();
    expect(parseViewMode({ mode: "admin" })).toBeNull();
  });
});

describe("restoreViewMode (the persisted-value re-gate — escalation vector)", () => {
  // This is the one place a client-persisted persona could escalate: a value lives
  // in the user's own localStorage, fully under their control, and is read back on
  // every load. The contract is that it is re-gated against the LIVE flag every
  // time and never trusted on its own.

  it("honours a stored admin home only while the principal is still an admin", () => {
    expect(restoreViewMode("admin", true)).toBe("admin");
    expect(restoreViewMode("user", true)).toBe("user");
  });

  it("collapses a stored admin home to user for a non-admin (stale or tampered)", () => {
    // An admin who has since been demoted re-reads as user.
    expect(restoreViewMode("admin", false)).toBe("user");
  });

  it("collapses a garbage stored value to user even for an admin", () => {
    expect(restoreViewMode("ops", true)).toBe("user");
    expect(restoreViewMode("ops", false)).toBe("user");
  });

  it("collapses a malformed/garbage stored value to user even for an admin", () => {
    expect(restoreViewMode("root", true)).toBe("user");
    expect(restoreViewMode("", true)).toBe("user");
    expect(restoreViewMode(null, true)).toBe("user");
    expect(restoreViewMode(42, true)).toBe("user");
  });
});

describe("sectionsForView (UX ceiling, composed on visibleSections)", () => {
  it("shows a non-admin only the User-Side regardless of the requested view", () => {
    for (const v of ["user", "admin"] as ViewMode[]) {
      expect(sectionsForView(v, false).map((s) => s.id)).toEqual(["user"]);
    }
  });

  it("foregrounds homes up to the chosen ceiling for an admin", () => {
    expect(sectionsForView("user", true).map((s) => s.id)).toEqual(["user"]);
    expect(sectionsForView("admin", true).map((s) => s.id)).toEqual(["user", "admin"]);
  });

  it("lets an admin step DOWN to the User-home and see only User-Side", () => {
    // The new capability the switcher adds: an admin can choose to view the app as a
    // plain user. visibleSections alone could never hide their admin nav; this can.
    const ids = sectionsForView("user", true).map((s) => s.id);
    expect(ids).toEqual(["user"]);
    expect(ids).not.toContain("admin");
  });

  it("never returns more than visibleSections already permits (subset invariant)", () => {
    // The switcher only ever narrows. For every (view, isAdmin) pair the result must
    // be a subset of visibleSections(isAdmin) — it can never widen access.
    for (const isAdmin of [true, false]) {
      const permitted = new Set(visibleSections(isAdmin).map((s) => s.id));
      for (const v of ["user", "admin"] as ViewMode[]) {
        for (const s of sectionsForView(v, isAdmin)) {
          expect(permitted.has(s.id)).toBe(true);
        }
      }
    }
  });
});
