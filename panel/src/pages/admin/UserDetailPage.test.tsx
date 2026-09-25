// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { UserDetailPage } from "./UserDetailPage";
import type { UserDetail } from "@/lib/types";

const calls = vi.hoisted(() => ({
  getUser: vi.fn(),
  getUserQuotas: vi.fn(),
  listUserSessions: vi.fn(),
}));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({ loading: false, identity: { user_id: "owner-1", role: "owner" }, isAdmin: true, isOwner: true }),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const VERIFIED = "2026-03-04T05:06:07Z";
const EXPIRES = "2026-11-12T13:14:15Z";

const USER: UserDetail = {
  id: "u-1",
  username: "steve",
  email: "steve@example.test",
  role: "user",
  disabled: false,
  email_verified: true,
  server_count: 0,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  linked_accounts: [{ mc_uuid: "069a79f4-44e9-4726-a5be-fca90e38aaf5", auth_source: "thirdparty", verified_at: VERIFIED }],
};

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/admin/users/u-1"]}>
      <Routes>
        <Route path="/admin/users/:id" element={<UserDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  calls.getUserQuotas.mockResolvedValue({ user_id: "u-1" });
  calls.listUserSessions.mockResolvedValue([]);
});
afterEach(() => i18next.changeLanguage("en-US"));

describe("UserDetailPage", () => {
  it("says the user is gone and leads back to the list, without the server's English", async () => {
    calls.getUser.mockRejectedValue({ status: 404, code: "not_found", message: "user not found" });
    await i18next.changeLanguage("zh-CN");
    renderPage();

    expect(await screen.findByText("用户不存在")).toBeTruthy();
    expect(screen.getByRole("link", { name: "返回用户列表" }).getAttribute("href")).toBe("/admin/users");
    expect(screen.queryByRole("button", { name: i18next.t("common:try_again") })).toBeNull();
    expect(document.body.textContent).not.toMatch(/user not found/i);
  });

  it("keeps the retry for a failure that is not a missing user", async () => {
    calls.getUser.mockRejectedValue({ status: 409, code: "test", message: "backend refused" });
    renderPage();

    expect((await screen.findByRole("alert")).textContent).toBe("backend refused");
    expect(screen.getByRole("button", { name: i18next.t("common:try_again") })).toBeTruthy();
    expect(screen.queryByText(i18next.t("admin:user_not_found_title"))).toBeNull();
  });

  it("names the auth source and writes dates in the UI language", async () => {
    calls.getUser.mockResolvedValue(USER);
    calls.listUserSessions.mockResolvedValue([
      { token_hash: "abcdef0123456789abcdef0123456789", created_at: VERIFIED, expires_at: EXPIRES },
    ]);
    await i18next.changeLanguage("zh-CN");
    renderPage();

    const zhVerified = new Date(VERIFIED).toLocaleString("zh-CN");
    const zhExpires = new Date(EXPIRES).toLocaleString("zh-CN");
    expect(zhVerified).not.toBe(new Date(VERIFIED).toLocaleString("en-US"));

    expect(await screen.findByText(`第三方 Yggdrasil · ${zhVerified}`)).toBeTruthy();
    expect(await screen.findByText(zhExpires, { exact: false })).toBeTruthy();
  });
});
