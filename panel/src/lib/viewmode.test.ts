import { describe, it, expect } from "vitest";
import {
  VIEW_MODES,
  availableViewModes,
  effectiveViewMode,
  landingPathForView,
  parseViewMode,
  restoreViewMode,
  sectionsForView,
  viewModeLabelKey,
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
    expect(availableViewModes(true)).toEqual(["user", "admin", "ops"]);
  });

  it("returns a fresh array so a caller cannot mutate the canonical list", () => {
    const a = availableViewModes(true);
    a.push("user");
    expect(availableViewModes(true)).toEqual(["user", "admin", "ops"]);
  });
});

describe("VIEW_MODES", () => {
  it("is the three homes ordered by how much they reveal", () => {
    expect(VIEW_MODES).toEqual(["user", "admin", "ops"]);
  });
});

describe("effectiveViewMode (fail-closed resolution)", () => {
  it("collapses any admin-level request from a non-admin to user", () => {
    expect(effectiveViewMode("admin", false)).toBe("user");
    expect(effectiveViewMode("ops", false)).toBe("user");
  });

  it("honours an admin's request for any home they are entitled to", () => {
    expect(effectiveViewMode("user", true)).toBe("user");
    expect(effectiveViewMode("admin", true)).toBe("admin");
    expect(effectiveViewMode("ops", true)).toBe("ops");
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
    expect(parseViewMode("ops")).toBe("ops");
  });

  it("rejects anything that is not a known home, returning null", () => {
    expect(parseViewMode("operator")).toBeNull();
    expect(parseViewMode("Admin")).toBeNull(); // case-sensitive on purpose
    expect(parseViewMode("")).toBeNull();
    expect(parseViewMode(null)).toBeNull();
    expect(parseViewMode(undefined)).toBeNull();
    expect(parseViewMode(2)).toBeNull();
    expect(parseViewMode({ mode: "ops" })).toBeNull();
  });
});

describe("restoreViewMode (the persisted-value re-gate — escalation vector)", () => {
  // This is the one place a client-persisted persona could escalate: a value lives
  // in the user's own localStorage, fully under their control, and is read back on
  // every load. The contract is that it is re-gated against the LIVE flag every
  // time and never trusted on its own.

  it("honours a stored admin/ops home only while the principal is still an admin", () => {
    expect(restoreViewMode("ops", true)).toBe("ops");
    expect(restoreViewMode("admin", true)).toBe("admin");
    expect(restoreViewMode("user", true)).toBe("user");
  });

  it("collapses a stored admin/ops home to user for a non-admin (stale or tampered)", () => {
    // A hand-edited localStorage "ops" on a non-admin account must NOT grant the
    // SysAdmin view; and a once-admin who has since been demoted re-reads as user.
    expect(restoreViewMode("ops", false)).toBe("user");
    expect(restoreViewMode("admin", false)).toBe("user");
  });

  it("collapses to user while the admin flag is still fail-closed false (loading)", () => {
    // Until /me resolves, isAdmin is false; a restored "ops" must wait at user, not
    // flash the SysAdmin home and then yank it back.
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
    for (const v of ["user", "admin", "ops"] as ViewMode[]) {
      expect(sectionsForView(v, false).map((s) => s.id)).toEqual(["user"]);
    }
  });

  it("foregrounds homes up to the chosen ceiling for an admin", () => {
    expect(sectionsForView("user", true).map((s) => s.id)).toEqual(["user"]);
    expect(sectionsForView("admin", true).map((s) => s.id)).toEqual(["user", "admin"]);
    expect(sectionsForView("ops", true).map((s) => s.id)).toEqual([
      "user",
      "admin",
      "ops",
    ]);
  });

  it("lets an admin step DOWN to the User-home and see only User-Side", () => {
    // The new capability the switcher adds: an admin can choose to view the app as a
    // plain user. visibleSections alone could never hide their admin nav; this can.
    const ids = sectionsForView("user", true).map((s) => s.id);
    expect(ids).toEqual(["user"]);
    expect(ids).not.toContain("admin");
    expect(ids).not.toContain("ops");
  });

  it("never returns more than visibleSections already permits (subset invariant)", () => {
    // The switcher only ever narrows. For every (view, isAdmin) pair the result must
    // be a subset of visibleSections(isAdmin) — it can never widen access.
    for (const isAdmin of [true, false]) {
      const permitted = new Set(visibleSections(isAdmin).map((s) => s.id));
      for (const v of ["user", "admin", "ops"] as ViewMode[]) {
        for (const s of sectionsForView(v, isAdmin)) {
          expect(permitted.has(s.id)).toBe(true);
        }
      }
    }
  });
});

describe("landingPathForView", () => {
  it("opens each home at its section root", () => {
    expect(landingPathForView("user")).toBe("/");
    expect(landingPathForView("admin")).toBe("/admin");
    expect(landingPathForView("ops")).toBe("/ops");
  });

  it("falls through to the User home for an out-of-band value", () => {
    expect(landingPathForView("nope" as unknown as ViewMode)).toBe("/");
  });

  it("targets a real, navigable path for every known home", () => {
    // Guards against a home being added without a landing route: each must resolve to
    // an absolute path the router can reach.
    for (const v of VIEW_MODES) {
      expect(landingPathForView(v)).toMatch(/^\//);
    }
  });
});

describe("viewModeLabelKey", () => {
  it("derives the navigation i18n key for each home", () => {
    expect(viewModeLabelKey("user")).toBe("view_user");
    expect(viewModeLabelKey("admin")).toBe("view_admin");
    expect(viewModeLabelKey("ops")).toBe("view_ops");
  });
});
