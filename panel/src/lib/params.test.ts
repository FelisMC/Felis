import { describe, it, expect } from "vitest";
import { SERVER_NAME_PARAM, USER_ID_PARAM } from "./params";

describe("route param shapes", () => {
  it("accepts real server names", () => {
    for (const name of ["lobby", "login", "test-one", "a1", "x"]) {
      expect(SERVER_NAME_PARAM.test(name), name).toBe(true);
    }
  });

  it("refuses names a crafted link would carry", () => {
    for (const name of ["..", ".", "", "a/b", "../users/x", "a?b", "a#b", "-a", "a-", "Lobby", "a b", "a".repeat(64)]) {
      expect(SERVER_NAME_PARAM.test(name), JSON.stringify(name)).toBe(false);
    }
  });

  it("accepts hex and UUID user ids, refuses path characters", () => {
    expect(USER_ID_PARAM.test("0123456789abcdef0123456789abcdef")).toBe(true);
    expect(USER_ID_PARAM.test("3f2b8c1e-9a4d-4c7e-8b1a-2d3e4f5a6b7c")).toBe(true);
    for (const id of ["..", ".", "", "a/b", "a?b", "a%2Fb", "a.b"]) {
      expect(USER_ID_PARAM.test(id), JSON.stringify(id)).toBe(false);
    }
  });
});
