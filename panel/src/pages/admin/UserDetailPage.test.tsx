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
  clearUserQuotas: vi.fn(),
  getSelfServiceQuota: vi.fn(),
  listUserSessions: vi.fn(),
  revokeUserSession: vi.fn(),
  revokeUserSessions: vi.fn(),
  fleet: vi.fn(),
  previewServerAssignment: vi.fn(),
  assignServer: vi.fn(),
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
  calls.getUserQuotas.mockResolvedValue({ user_id: "u-1", custom: false });
  calls.getSelfServiceQuota.mockResolvedValue({ enabled: false, max_servers: null, max_cpu_milli: null, max_memory_mb: null, max_storage_gb: null, revision: "" });
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

  describe("assigning a server", () => {
    const assignButton = () => screen.getByRole("button", { name: i18next.t("admin:assign_title") }) as HTMLButtonElement;
    const server = (name: string, over: Record<string, unknown>) => ({ name, subdomain: name, phase: "Stopped", ready: false, playersOnline: 0, playersMax: 20, ...over });

    it("gives the server to this account and refreshes the page", async () => {
      calls.getUser.mockResolvedValue(USER);
      calls.fleet.mockResolvedValue([
        server("survival", { owner: "owner@example.test" }),
        server("owned", { owner: "steve@example.test" }),
        server("lobby", { system: true }),
      ]);
      calls.previewServerAssignment.mockResolvedValue({ name: "survival", owner_id: "owner-1", over: [] });
      calls.assignServer.mockResolvedValue({ name: "survival", owner_id: "u-1", over: [] });
      renderPage();
      const user = userEvent.setup();

      await user.click(await screen.findByRole("button", { name: i18next.t("admin:assign_title") }));
      const dialog = await screen.findByRole("dialog");
      expect(await within(dialog).findByRole("button", { name: /survival/ })).toBeTruthy();
      expect(within(dialog).queryByRole("button", { name: /owned/ })).toBeNull();
      expect(within(dialog).queryByRole("button", { name: /lobby/ })).toBeNull();

      await user.click(within(dialog).getByRole("button", { name: /survival/ }));
      const confirm = await within(dialog).findByRole("button", { name: i18next.t("admin:assign_confirm") });
      await waitFor(() => expect((confirm as HTMLButtonElement).disabled).toBe(false));
      await user.click(confirm);

      expect(calls.assignServer).toHaveBeenCalledExactlyOnceWith("survival", { user_id: "u-1", allow_over_quota: false });
      await waitFor(() => expect(calls.getUser).toHaveBeenCalledTimes(2));
    });

    it("is not offered for a disabled account", async () => {
      calls.getUser.mockResolvedValue({ ...USER, disabled: true });
      renderPage();

      await screen.findByText("steve");
      expect(assignButton().disabled).toBe(true);
    });
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
      expect(screen.queryByText("The session has ended or does not exist.")).toBeNull();
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
    const SELF_REASON = "The signed-in account cannot be disabled or deleted.";
    const OWNER_REASON =
      "The Owner account cannot be disabled or deleted from the panel. Manage it through the host recovery console (sudo felis breakGlass).";

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
      expect(screen.getByText("The current account’s role cannot be changed.")).toBeTruthy();
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
          "The Owner role cannot be changed. Manage it through the host recovery console (sudo felis breakGlass).",
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
  const saveQuota = () => screen.getByRole("button", { name: "Save quota" }) as HTMLButtonElement;
  const DEFAULTS = { enabled: true, max_servers: 2, max_cpu_milli: null, max_memory_mb: 8192, max_storage_gb: 30, revision: "r1" };

  it("tells a user on the default quota what it allows, and gives them caps of their own in GB and cores", async () => {
    calls.getUser.mockResolvedValue(USER);
    calls.getSelfServiceQuota.mockResolvedValue(DEFAULTS);
    calls.setUserQuotas.mockResolvedValue({ user_id: "u-1", custom: true, max_servers: 3, max_memory_mb: 4096 });
    renderPage();
    const user = userEvent.setup();

    expect(await screen.findByText("This user is held to the default quota: servers 2, memory 8 GB, CPU Unlimited, storage 30 GB.")).toBeTruthy();
    expect(screen.getByRole("radio", { name: "Default quota" }).getAttribute("aria-checked")).toBe("true");
    expect(saveQuota().disabled).toBe(true);

    await user.click(screen.getByRole("radio", { name: "Own quota" }));
    await user.type(screen.getByLabelText("Servers"), "3");
    await user.type(screen.getByLabelText("Memory (GB)"), "4");
    await user.type(screen.getByLabelText("CPU (cores)"), "1.5");
    await user.click(saveQuota());

    await waitFor(() => expect(calls.setUserQuotas).toHaveBeenCalledTimes(1));
    expect(calls.setUserQuotas).toHaveBeenCalledWith("u-1", { max_servers: 3, max_memory_mb: 4096, max_cpu_milli: 1500, max_storage_gb: null });
    expect(calls.clearUserQuotas).not.toHaveBeenCalled();
  });

  it("returns a user with an own quota to the default by removing it", async () => {
    calls.getUser.mockResolvedValue(USER);
    calls.getSelfServiceQuota.mockResolvedValue(DEFAULTS);
    calls.getUserQuotas.mockResolvedValue({ user_id: "u-1", custom: true, max_servers: 5, max_memory_mb: 16384 });
    calls.clearUserQuotas.mockResolvedValue({ user_id: "u-1", custom: false });
    renderPage();
    const user = userEvent.setup();

    await waitFor(() => expect((screen.getByLabelText("Memory (GB)") as HTMLInputElement).value).toBe("16"));
    expect((screen.getByLabelText("Servers") as HTMLInputElement).value).toBe("5");
    await user.click(screen.getByRole("radio", { name: "Default quota" }));
    await user.click(saveQuota());

    await waitFor(() => expect(calls.clearUserQuotas).toHaveBeenCalledWith("u-1"));
    expect(calls.setUserQuotas).not.toHaveBeenCalled();
    expect(await screen.findByText("This user is now held to the default quota.")).toBeTruthy();
  });

  it("says what a user without a quota may do while self-service is off", async () => {
    calls.getUser.mockResolvedValue(USER);
    calls.getSelfServiceQuota.mockResolvedValue({ ...DEFAULTS, enabled: false });
    renderPage();
    expect(await screen.findByText(/Self-service servers are off: this user cannot create servers/)).toBeTruthy();
  });

  it("refuses caps that are not non-negative numbers, and sends nothing", async () => {
    calls.getUser.mockResolvedValue(USER);
    calls.getSelfServiceQuota.mockResolvedValue(DEFAULTS);
    renderPage();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("radio", { name: "Own quota" }));

    for (const [field, bad] of [["Memory (GB)", "-1"], ["Memory (GB)", "2abc"], ["Servers", "1.5"]] as const) {
      await user.clear(screen.getByLabelText(field));
      await user.type(screen.getByLabelText(field), bad);
      expect(screen.getByText("Enter non-negative numbers; the server count must be a whole number.")).toBeTruthy();
      expect(saveQuota().disabled).toBe(true);
      await user.clear(screen.getByLabelText(field));
    }
    expect(calls.setUserQuotas).not.toHaveBeenCalled();
  });
});
