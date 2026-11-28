import { describe, it, expect } from "vitest";
import { deriveServerSlug, serverNameIssue } from "./naming";

describe("deriveServerSlug", () => {
  it.each([
    ["Survival World", "survival-world"],
    ["Survival-2", "survival-2"],
    ["  SMP_01! ", "smp-01"],
    ["Café Realm", "cafe-realm"],
    ["生存服", ""],
    ["我的 Survival 服", "survival"],
    ["a".repeat(40), "a".repeat(32)],
    [`${"a".repeat(31)}-tail`, "a".repeat(31)],
  ])("derives %j -> %j", (input, expected) => {
    expect(deriveServerSlug(input)).toBe(expected);
  });

  it("pairs with serverNameIssue to reject short or reserved derived slugs", () => {
    expect(serverNameIssue(deriveServerSlug("生存服"))).toBe("shape");
    expect(serverNameIssue(deriveServerSlug("AB"))).toBe("shape");
    expect(serverNameIssue(deriveServerSlug("Lobby"))).toBe("reserved");
    expect(serverNameIssue(deriveServerSlug("Survival World"))).toBeNull();
  });
});
