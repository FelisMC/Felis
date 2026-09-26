// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { UserDetailPage } from "./UserDetailPage";
import type { UserDetail } from "@/lib/types";

const calls = vi.hoisted(() => ({
  getUser: vi.fn(),
  getUserQuotas: vi.fn(),
  setUserQuotas: vi.fn(),
  listUserSessions: vi.fn(),
  revokeUserSession: vi.fn(),
  revokeUserSessions: vi.fn(),
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

function renderPage(id = "u-1") {
  return render(
    <MemoryRouter initialEntries={[`/admin/users/${id}`]}>
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
      {
        token_hash: "abcdef0123456789abcdef0123456789",
        created_at: VERIFIED,
        expires_at: EXPIRES,
        last_seen_at: VERIFIED,
        user_agent: "",
        client_ip: "",
      },
    ]);
    await i18next.changeLanguage("zh-CN");
    renderPage();

    const zhVerified = new Date(VERIFIED).toLocaleString("zh-CN");
    const zhExpires = new Date(EXPIRES).toLocaleString("zh-CN");
    expect(zhVerified).not.toBe(new Date(VERIFIED).toLocaleString("en-US"));

    expect(await screen.findByText(`第三方 Yggdrasil · ${zhVerified}`)).toBeTruthy();
    expect(await screen.findByText(zhExpires, { exact: false })).toBeTruthy();
  });

  describe("sessions card", () => {
    const seen = new Date(Date.now() - 2 * 3_600_000).toISOString();
    const phone = {
      token_hash: "h-phone",
      created_at: VERIFIED,
      expires_at: EXPIRES,
      last_seen_at: seen,
      user_agent:
        "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Mobile Safari/537.36",
      client_ip: "198.51.100.9",
    };
    const pc = { ...phone, token_hash: "h-pc", user_agent: "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Firefox/130.0", client_ip: "" };

    it("names each device with its address and last activity", async () => {
      calls.getUser.mockResolvedValue(USER);
      calls.listUserSessions.mockResolvedValue([phone, pc]);
      renderPage();

      const label = await screen.findByText("Chrome on Android");
      const row = label.closest("div.rounded-md") as HTMLElement;
      expect(label.getAttribute("title")).toBe(phone.user_agent);
      expect(within(row).getByText("198.51.100.9")).toBeTruthy();
      expect(within(row).getByText("Active 2 hours ago")).toBeTruthy();
      expect(screen.getByText("Firefox on Linux")).toBeTruthy();
      expect(screen.getByRole("button", { name: "Revoke the session on Chrome on Android" })).toBeTruthy();
      expect(screen.getByRole("button", { name: "Revoke the session on Firefox on Linux" })).toBeTruthy();
    });

    it("closes the confirmation and refreshes when the session had already ended", async () => {
      calls.getUser.mockResolvedValue(USER);
      calls.listUserSessions.mockResolvedValueOnce([phone, pc]).mockResolvedValue([pc]);
      calls.revokeUserSession.mockRejectedValue({ status: 404, code: "session_not_found", message: "gone" });
      renderPage();

      await userEvent.click(await screen.findByRole("button", { name: "Revoke the session on Chrome on Android" }));
      await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Revoke" }));

      expect(calls.revokeUserSession).toHaveBeenCalledWith("u-1", "h-phone");
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      await waitFor(() => expect(screen.queryByText("Chrome on Android")).toBeNull());
      expect(screen.queryByText("That session has already ended.")).toBeNull();
    });

    it("shows why a revoke failed for any other reason", async () => {
      calls.getUser.mockResolvedValue(USER);
      calls.listUserSessions.mockResolvedValue([phone]);
      calls.revokeUserSession.mockRejectedValue({ status: 409, code: "test", message: "backend refused" });
      renderPage();

      await userEvent.click(await screen.findByRole("button", { name: "Revoke the session on Chrome on Android" }));
      await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Revoke" }));

      // Inside the dialog: the modal hides the card behind it.
      const dialog = screen.getByRole("dialog");
      expect((await within(dialog).findByRole("alert")).textContent).toBe("backend refused");
      await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
      expect(screen.getByText("Chrome on Android")).toBeTruthy();

      // Asking again starts clean, without the last attempt's failure.
      await userEvent.click(screen.getByRole("button", { name: "Revoke the session on Chrome on Android" }));
      expect(within(screen.getByRole("dialog")).queryByRole("alert")).toBeNull();
    });

    it("shows why revoking every session failed inside the confirmation", async () => {
      calls.getUser.mockResolvedValue(USER);
      calls.listUserSessions.mockResolvedValue([phone]);
      calls.revokeUserSessions.mockRejectedValue({ status: 409, code: "test", message: "backend refused" });
      renderPage();

      await userEvent.click(await screen.findByRole("button", { name: "Revoke All" }));
      const dialog = screen.getByRole("dialog");
      await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));

      expect((await within(dialog).findByRole("alert")).textContent).toBe("backend refused");
    });
  });

  describe("danger zone for accounts the server protects", () => {
    const SELF_REASON = "You can't disable or delete the account you're signed in with.";
    const OWNER_REASON =
      "The owner account can't be disabled or deleted from the panel. Only the host's break-glass console (sudo felis breakGlass) manages it.";

    const button = (name: string) => screen.getByRole("button", { name }) as HTMLButtonElement;
    const describedBy = (b: HTMLElement) =>
      document.getElementById(b.getAttribute("aria-describedby") ?? "")?.textContent;

    it("locks disable and delete on the signed-in owner's own row and says why", async () => {
      calls.getUser.mockResolvedValue({ ...USER, id: "owner-1", username: "root", role: "owner" });
      renderPage("owner-1");

      expect(await screen.findByText(SELF_REASON)).toBeTruthy();
      for (const name of ["Disable User", "Delete User"]) {
        expect(button(name).disabled).toBe(true);
        expect(describedBy(button(name))).toBe(SELF_REASON);
        expect(button(name).parentElement?.getAttribute("title")).toBe(SELF_REASON);
      }
      expect(button("Unbind passkeys").disabled).toBe(false);
      expect(screen.getByText("You can't change your own role.")).toBeTruthy();
      expect(screen.queryByRole("combobox")).toBeNull();
    });

    it("locks disable and delete on another owner with the break-glass reason", async () => {
      calls.getUser.mockResolvedValue({ ...USER, id: "owner-2", username: "co-owner", role: "owner" });
      renderPage("owner-2");

      expect(await screen.findByText(OWNER_REASON)).toBeTruthy();
      expect(screen.queryByText(SELF_REASON)).toBeNull();
      expect(button("Disable User").disabled).toBe(true);
      expect(button("Delete User").disabled).toBe(true);
      expect(describedBy(button("Delete User"))).toBe(OWNER_REASON);
      expect(
        screen.getByText(
          "The owner's role is fixed. Only the host's break-glass console (sudo felis breakGlass) manages the owner.",
        ),
      ).toBeTruthy();
      expect(screen.queryByRole("combobox")).toBeNull();
    });

    it("still lets a disabled owner be re-enabled", async () => {
      calls.getUser.mockResolvedValue({ ...USER, id: "owner-2", role: "owner", disabled: true });
      renderPage("owner-2");

      expect((await screen.findByRole("button", { name: "Enable" }) as HTMLButtonElement).disabled).toBe(false);
      expect(button("Enable").getAttribute("aria-describedby")).toBeNull();
      expect(button("Delete User").disabled).toBe(true);
    });

    it("leaves every action open on an ordinary user", async () => {
      calls.getUser.mockResolvedValue(USER);
      renderPage();

      expect((await screen.findByRole("button", { name: "Disable User" }) as HTMLButtonElement).disabled).toBe(false);
      expect(button("Delete User").disabled).toBe(false);
      expect(button("Delete User").parentElement?.getAttribute("title")).toBeNull();
      expect(screen.queryByText(SELF_REASON)).toBeNull();
      expect(screen.queryByText(OWNER_REASON)).toBeNull();
      const role = screen.getByRole("combobox");
      expect(within(role).getByText("User")).toBeTruthy();
    });
  });
});

describe("UserDetailPage quotas", () => {
  function quotaBox(label: string) {
    return screen.getByLabelText(i18next.t(`admin:${label}`)) as HTMLInputElement;
  }
  function quotaCard() {
    return quotaBox("users_quota_servers").closest(".space-y-4") as HTMLElement;
  }

  it("sends every box, an emptied one as unlimited and 0 as none", async () => {
    calls.getUser.mockResolvedValue(USER);
    calls.getUserQuotas.mockResolvedValue({ user_id: "u-1", max_servers: 3, max_cpu_milli: 2000 });
    calls.setUserQuotas.mockResolvedValue({ user_id: "u-1", max_cpu_milli: 2000, max_storage_gb: 0 });
    renderPage();
    const user = userEvent.setup();

    await waitFor(() => expect(quotaBox("users_quota_servers").value).toBe("3"));
    expect(within(quotaCard()).getByText(i18next.t("admin:users_quota_hint"))).toBeTruthy();
    await user.clear(quotaBox("users_quota_servers"));
    await user.type(quotaBox("users_quota_storage"), "0");
    await user.click(within(quotaCard()).getByRole("button", { name: i18next.t("admin:users_save_btn") }));

    await waitFor(() => expect(calls.setUserQuotas).toHaveBeenCalledTimes(1));
    expect(calls.setUserQuotas).toHaveBeenCalledWith("u-1", {
      max_servers: null,
      max_cpu_milli: 2000,
      max_memory_mb: null,
      max_storage_gb: 0,
    });
  });

  it("refuses a box that is not a whole number the server can store, and sends nothing", async () => {
    calls.getUser.mockResolvedValue(USER);
    renderPage();
    const user = userEvent.setup();
    await waitFor(() => expect(quotaBox("users_quota_servers")).toBeTruthy());

    for (const bad of ["-1", "1.5", "2abc", "2147483648"]) {
      await user.clear(quotaBox("users_quota_memory"));
      await user.type(quotaBox("users_quota_memory"), bad);
      await user.click(within(quotaCard()).getByRole("button", { name: i18next.t("admin:users_save_btn") }));
      expect(await within(quotaCard()).findByText(i18next.t("errors:invalid_quota"))).toBeTruthy();
      expect(quotaBox("users_quota_memory").value).toBe(bad);
    }
    expect(calls.setUserQuotas).not.toHaveBeenCalled();

    await user.clear(quotaBox("users_quota_memory"));
    await user.type(quotaBox("users_quota_memory"), " 2147483647 ");
    calls.setUserQuotas.mockResolvedValue({ user_id: "u-1", max_memory_mb: 2147483647 });
    await user.click(within(quotaCard()).getByRole("button", { name: i18next.t("admin:users_save_btn") }));
    await waitFor(() => expect(calls.setUserQuotas).toHaveBeenCalledTimes(1));
    expect(calls.setUserQuotas.mock.calls[0][1].max_memory_mb).toBe(2147483647);
  });
});
