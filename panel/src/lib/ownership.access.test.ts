import { describe, expect, it } from "vitest";
import {
  canDeleteServer,
  canManage,
  canManageRole,
  canOperateRole,
  isReadonlyRole,
  resolveServerAccessRole,
} from "./ownership";
import type { MyServerView } from "./types";

function myRow(name: string, over: Partial<MyServerView> = {}): MyServerView {
  return {
    name,
    subdomain: name,
    owned: false,
    claimable: false,
    playersOnline: 0,
    playersMax: 20,
    ...over,
  };
}

describe("resolveServerAccessRole", () => {
  it("prefers status.accessRole over mine and isAdmin fallback", () => {
    expect(resolveServerAccessRole(true, { accessRole: "readonly" }, [myRow("srv", { owned: true })], "srv")).toBe("readonly");
    expect(resolveServerAccessRole(false, { accessRole: "operator" }, [myRow("srv", { accessRole: "manager" })], "srv")).toBe("operator");
  });

  it("falls back to mine entry accessRole or owned", () => {
    expect(resolveServerAccessRole(false, undefined, [myRow("srv", { accessRole: "manager" })], "srv")).toBe("manager");
    expect(resolveServerAccessRole(false, undefined, [myRow("srv", { owned: true })], "srv")).toBe("owner");
  });

  it("falls back to admin or none when status and mine have no match", () => {
    expect(resolveServerAccessRole(true, undefined, [], "srv")).toBe("admin");
    expect(resolveServerAccessRole(false, undefined, [myRow("other", { owned: true })], "srv")).toBe("none");
  });
});

describe("role capability helpers", () => {
  it("distinguishes readonly, operator, manager, admin, owner", () => {
    expect(canOperateRole("owner")).toBe(true);
    expect(canOperateRole("admin")).toBe(true);
    expect(canOperateRole("manager")).toBe(true);
    expect(canOperateRole("operator")).toBe(true);
    expect(canOperateRole("readonly")).toBe(false);
    expect(canOperateRole("none")).toBe(false);

    expect(canManageRole("owner")).toBe(true);
    expect(canManageRole("admin")).toBe(true);
    expect(canManageRole("manager")).toBe(true);
    expect(canManageRole("operator")).toBe(false);
    expect(canManageRole("readonly")).toBe(false);
    expect(canManageRole("none")).toBe(false);

    expect(isReadonlyRole("readonly")).toBe(true);
    expect(isReadonlyRole("operator")).toBe(false);
  });

  it("allows delete for owner always and admin only when unprotected", () => {
    expect(canDeleteServer(true, true, true)).toBe(true);
    expect(canDeleteServer(false, true, false)).toBe(true);
    expect(canDeleteServer(false, true, true)).toBe(false);
    expect(canDeleteServer(false, false, false)).toBe(false);
  });

  it("recognizes manager collaborator rows in legacy canManage", () => {
    expect(canManage(false, [myRow("srv", { accessRole: "manager" })], "srv")).toBe(true);
    expect(canManage(false, [myRow("srv", { accessRole: "operator" })], "srv")).toBe(false);
  });
});
