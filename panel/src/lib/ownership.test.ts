import { describe, it, expect } from "vitest";
import { canManage, ownershipPending } from "./ownership";
import type { MyServerView } from "./types";

// The owner-tier gate once read `owned` off /servers/{name}/status, which never
// sends it, so owners lost the LuckPerms entry. These cases pin that ownership
// comes only from the /me/servers row for this exact server.

const row = (name: string, owned = false): MyServerView => ({
  name,
  subdomain: name,
  phase: "Running",
  owned,
  claimable: false,
  playersOnline: 0,
  playersMax: 0,
});

describe("canManage", () => {
  it("lets an admin in without any /me/servers rows", () => {
    expect(canManage(true, null, "lobby")).toBe(true);
  });

  it("lets the owner in from their /me/servers row", () => {
    expect(canManage(false, [row("other", false), row("lobby", true)], "lobby")).toBe(true);
  });

  it("keeps a non-owner out", () => {
    expect(canManage(false, [row("lobby", false)], "lobby")).toBe(false);
    expect(canManage(false, [row("lobby")], "lobby")).toBe(false);
    expect(canManage(false, [row("other", true)], "lobby")).toBe(false);
  });

  it("keeps everyone out while /me/servers is unanswered", () => {
    expect(canManage(false, null, "lobby")).toBe(false);
    expect(canManage(false, undefined, "lobby")).toBe(false);
  });
});

describe("ownershipPending", () => {
  it("waits for the tier", () => {
    expect(ownershipPending(true, false, [], null)).toBe(true);
  });

  it("waits for a non-admin's /me/servers", () => {
    expect(ownershipPending(false, false, null, null)).toBe(true);
    expect(ownershipPending(false, false, [], null)).toBe(false);
  });

  it("stops waiting once the read failed, so the page can offer a retry", () => {
    expect(ownershipPending(false, false, null, { status: 503 })).toBe(false);
  });

  it("never waits on an admin", () => {
    expect(ownershipPending(false, true, null, null)).toBe(false);
  });
});
