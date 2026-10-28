import { describe, it, expect } from "vitest";
import { NAV_SECTIONS, visibleSections } from "./nav";


describe("visibleSections", () => {
  it("shows only the User-Side section to a non-admin", () => {
    const ids = visibleSections(false, false).map((s) => s.id);
    expect(ids).toEqual(["user"]);
  });

  it("treats the fail-closed default (false) exactly like a non-admin", () => {
    // TierProvider passes false while /me is loading or after it rejects; that path must collapse to User-Side only, never leak Admin or Ops nav.
    expect(visibleSections(false, false)).toHaveLength(1);
    expect(visibleSections(false, false)[0].id).toBe("user");
  });

  it("shows User-Side and Admin-Side to an admin", () => {
    const ids = visibleSections(true, false).map((s) => s.id);
    expect(ids).toEqual(["user", "admin"]);
  });

  it("places platform maintenance exclusively in Owner navigation, before platform settings", () => {
    expect(visibleSections(true, false).flatMap((section) => section.items).some((item) => item.to === "/admin/updates")).toBe(false);
    const owner = visibleSections(true, true).find((section) => section.id === "owner");
    expect(owner?.items.map((item) => item.to)).toContain("/admin/updates");
    expect(owner?.items.at(-1)?.to).toBe("/admin/platform");
  });

  it("lists the audit log for the Owner and not for an admin", () => {
    const links = (isAdmin: boolean, isOwner: boolean) =>
      visibleSections(isAdmin, isOwner).flatMap((section) => section.items.map((item) => item.to));
    expect(links(true, false)).not.toContain("/admin/audit");
    expect(links(true, true)).toContain("/admin/audit");
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
