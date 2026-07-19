import { describe, it, expect } from "vitest";
import { deriveAuth, isUnauthorized } from "./auth";
import type { Identity } from "./types";

// deriveAuth is the load-bearing auth decision: it decides who is bounced to /login
// and — critically — who is KEPT in
// the app despite a /me failure. The one distinction that must never blur is a true
// 401 (no session → login) versus any other failure (transient → stay functional),
// because mistaking the latter for the former would log out a healthy Zero-Trust
// principal on a single flaky request. These cases pin every branch.

const admin: Identity = {
  user_id: "u1",
  email: "a@b.c",
  role: "admin",
  is_admin: true,
  is_owner: false,
};

const err401 = { status: 401, code: "unauthorized", message: "no session" };
const err500 = { status: 500, code: "error", message: "boom" };

describe("isUnauthorized", () => {
  it("is true only for a 401 envelope", () => {
    expect(isUnauthorized(err401)).toBe(true);
  });

  it("is false for any non-401 failure (transient, 5xx, network)", () => {
    expect(isUnauthorized(err500)).toBe(false);
    expect(isUnauthorized(new TypeError("Failed to fetch"))).toBe(false);
    expect(isUnauthorized(null)).toBe(false);
    expect(isUnauthorized(undefined)).toBe(false);
    expect(isUnauthorized("nope")).toBe(false);
  });
});

describe("deriveAuth", () => {
  it("while loading: never unauthenticated, never admin, regardless of error", () => {
    const s = deriveAuth(null, err401, true);
    expect(s.loading).toBe(true);
    expect(s.unauthenticated).toBe(false);
    expect(s.isAdmin).toBe(false);
  });

  it("a settled 401 with no identity is unauthenticated (→ /login)", () => {
    const s = deriveAuth(null, err401, false);
    expect(s.unauthenticated).toBe(true);
    expect(s.isAdmin).toBe(false);
  });

  it("a settled NON-401 failure is NOT unauthenticated (graded ZT stays functional)", () => {
    const s = deriveAuth(null, err500, false);
    expect(s.unauthenticated).toBe(false);
    // identity is null so admin surfaces stay hidden, but the app keeps rendering.
    expect(s.isAdmin).toBe(false);
  });

  it("a loaded admin identity is admin and authenticated", () => {
    const s = deriveAuth(admin, null, false);
    expect(s.unauthenticated).toBe(false);
    expect(s.isAdmin).toBe(true);
  });

  it("fails closed on a malformed identity missing is_admin", () => {
    // Mirrors the wire-shape trap: absent fields are undefined, not thrown access.
    const partial = { user_id: "u", email: "e", role: "user" } as unknown as Identity;
    const s = deriveAuth(partial, null, false);
    expect(s.isAdmin).toBe(false);
    expect(s.unauthenticated).toBe(false);
  });
});
