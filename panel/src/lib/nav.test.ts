import { describe, it, expect } from "vitest";
import { NAV_SECTIONS, visibleSections } from "./nav";

// visibleSections is the load-bearing tier decision: get it wrong and either an
// admin loses their tools or a non-admin sees admin nav. These cases pin the
// fail-closed default (loading/error → false → User-Side only) and the full-admin
// expansion, and guard the invariant that User-Side is never admin-gated.

describe("visibleSections", () => {
  it("shows only the User-Side section to a non-admin", () => {
    const ids = visibleSections(false, false).map((s) => s.id);
    expect(ids).toEqual(["user"]);
  });

  it("treats the fail-closed default (false) exactly like a non-admin", () => {
    // TierProvider passes `false` while /me is loading or after it rejects. That
    // path MUST collapse to User-Side only, never leak Admin/Ops nav.
    expect(visibleSections(false, false)).toHaveLength(1);
    expect(visibleSections(false, false)[0].id).toBe("user");
  });

  it("shows User-Side and Admin-Side to an admin", () => {
    const ids = visibleSections(true, false).map((s) => s.id);
    expect(ids).toEqual(["user", "admin"]);
  });

  it("keeps the User-Side section ungated so it survives both branches", () => {
    const user = NAV_SECTIONS.find((s) => s.id === "user");
    expect(user?.adminOnly).toBe(false);
    expect(visibleSections(true, false)).toContainEqual(user);
    expect(visibleSections(false, false)).toContainEqual(user);
  });

  it("gates every non-user section behind admin or owner", () => {
    for (const s of NAV_SECTIONS) {
      if (s.id !== "user") {
        expect(s.adminOnly || s.ownerOnly).toBe(true);
      }
    }
  });
});
