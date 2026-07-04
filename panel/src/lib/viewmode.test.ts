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

describe("availableViewModes", () => {
  it("offers a non-admin exactly the User-Side home", () => {
    expect(availableViewModes(false, false)).toEqual(["user"]);
  });

  it("treats the fail-closed default (false) exactly like a non-admin", () => {
    expect(availableViewModes(false, false)).toEqual(["user"]);
  });

  it("offers an admin every home except owner", () => {
    expect(availableViewModes(true, false)).toEqual(["user", "admin"]);
  });

  it("offers an owner every home including owner", () => {
    expect(availableViewModes(true, true)).toEqual(["user", "admin", "owner"]);
  });
});

describe("VIEW_MODES", () => {
  it("is the three homes ordered by how much they reveal", () => {
    expect(VIEW_MODES).toEqual(["user", "admin", "owner"]);
  });
});

describe("effectiveViewMode (fail-closed resolution)", () => {
  it("collapses any admin-level request from a non-admin to user", () => {
    expect(effectiveViewMode("admin", false, false)).toBe("user");
  });

  it("honours an admin's request for any home they are entitled to", () => {
    expect(effectiveViewMode("user", true, false)).toBe("user");
    expect(effectiveViewMode("admin", true, false)).toBe("admin");
    expect(effectiveViewMode("owner", true, false)).toBe("user"); // admin not entitled to owner
  });

  it("honours an owner's request for any home they are entitled to", () => {
    expect(effectiveViewMode("user", true, true)).toBe("user");
    expect(effectiveViewMode("admin", true, true)).toBe("admin");
    expect(effectiveViewMode("owner", true, true)).toBe("owner");
  });

  it("defaults a null/undefined request to the user home for either tier", () => {
    expect(effectiveViewMode(null, true, false)).toBe("user");
    expect(effectiveViewMode(undefined, true, false)).toBe("user");
    expect(effectiveViewMode(null, false, false)).toBe("user");
  });
});

describe("parseViewMode (shape only, no gating)", () => {
  it("accepts each known home verbatim", () => {
    expect(parseViewMode("user")).toBe("user");
    expect(parseViewMode("admin")).toBe("admin");
    expect(parseViewMode("owner")).toBe("owner");
  });

  it("rejects anything that is not a known home, returning null", () => {
    expect(parseViewMode("ops")).toBeNull();
    expect(parseViewMode("operator")).toBeNull();
    expect(parseViewMode("Admin")).toBeNull();
    expect(parseViewMode("")).toBeNull();
    expect(parseViewMode(null)).toBeNull();
    expect(parseViewMode(undefined)).toBeNull();
    expect(parseViewMode(2)).toBeNull();
    expect(parseViewMode({ mode: "admin" })).toBeNull();
  });
});

describe("restoreViewMode (the persisted-value re-gate — escalation vector)", () => {
  it("honours a stored admin home only while the principal is still an admin", () => {
    expect(restoreViewMode("admin", true, false)).toBe("admin");
    expect(restoreViewMode("user", true, false)).toBe("user");
  });

  it("collapses a stored admin home to user for a non-admin (stale or tampered)", () => {
    expect(restoreViewMode("admin", false, false)).toBe("user");
  });

  it("collapses a stored owner home to user for a non-owner", () => {
    expect(restoreViewMode("owner", true, false)).toBe("user");
  });

  it("honours a stored owner home for an owner", () => {
    expect(restoreViewMode("owner", true, true)).toBe("owner");
  });
});

describe("sectionsForView (UX ceiling, composed on visibleSections)", () => {
  it("shows a non-admin only the User-Side regardless of the requested view", () => {
    for (const v of ["user", "admin", "owner"] as ViewMode[]) {
      expect(sectionsForView(v, false, false).map((s) => s.id)).toEqual(["user"]);
    }
  });

  it("foregrounds homes up to the chosen ceiling for an admin", () => {
    expect(sectionsForView("user", true, false).map((s) => s.id)).toEqual(["user"]);
    expect(sectionsForView("admin", true, false).map((s) => s.id)).toEqual(["user", "admin"]);
  });

  it("foregrounds homes up to the chosen ceiling for an owner", () => {
    expect(sectionsForView("user", true, true).map((s) => s.id)).toEqual(["user"]);
    expect(sectionsForView("admin", true, true).map((s) => s.id)).toEqual(["user", "admin"]);
    expect(sectionsForView("owner", true, true).map((s) => s.id)).toEqual(["user", "admin", "owner"]);
  });

  it("never returns more than visibleSections already permits (subset invariant)", () => {
    for (const isOwner of [true, false]) {
      for (const isAdmin of [true, false]) {
        const permitted = new Set(visibleSections(isAdmin, isOwner).map((s) => s.id));
        for (const v of ["user", "admin", "owner"] as ViewMode[]) {
          for (const s of sectionsForView(v, isAdmin, isOwner)) {
            expect(permitted.has(s.id)).toBe(true);
          }
        }
      }
    }
  });
});
