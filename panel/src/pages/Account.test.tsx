// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import type { Identity, PasskeyCredential, SessionView } from "@/lib/types";
import { Account } from "./Account";

const mocks = vi.hoisted(() => ({
  passkeyList: vi.fn(),
  passkeyDelete: vi.fn(),
  listMySessions: vi.fn(),
  migrateStatus: vi.fn(),
  migrateIssueCode: vi.fn(),
  identity: null as Identity | null,
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      linkStatus: () => Promise.resolve({ linked: true }),
      migrateStatus: mocks.migrateStatus,
      migrateIssueCode: mocks.migrateIssueCode,
      passkeyList: mocks.passkeyList,
      passkeyDelete: mocks.passkeyDelete,
      listMySessions: mocks.listMySessions,
    },
  };
});
vi.mock("@/lib/tier", () => ({
  useTier: () => ({ identity: mocks.identity, refresh: vi.fn() }),
}));

const t = (key: string, opts?: Record<string, unknown>) => i18next.t(key, opts);
const deleteButton = (name: string) => ({ name: t("account:passkey_delete_aria", { name }) });

const laptop: PasskeyCredential = { id: "pk-1", name: "Laptop", created_at: "2026-03-01T10:00:00Z" };
const phone: PasskeyCredential = { id: "pk-2", name: "Phone", created_at: "2026-04-01T10:00:00Z" };

function session(hash: string, userAgent: string, current?: boolean): SessionView {
  const now = new Date().toISOString();
  return { token_hash: hash, created_at: now, expires_at: now, last_seen_at: now, user_agent: userAgent, client_ip: "", current };
}
const thisMac = session("h-mac", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/128.0.0.0 Safari/537.36", true);
const otherPC = session("h-pc", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Firefox/130.0");

function identity(emailVerified: boolean): Identity {
  return {
    user_id: "u-1",
    email: "a@example.com",
    role: "user",
    is_admin: false,
    is_owner: false,
    email_verified: emailVerified,
  };
}

function renderAccount() {
  return render(
    <MemoryRouter>
      <Account />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mocks.passkeyList.mockReset();
  mocks.passkeyDelete.mockReset();
  mocks.listMySessions.mockReset();
  mocks.listMySessions.mockResolvedValue([thisMac]);
  mocks.migrateStatus.mockReset();
  mocks.migrateStatus.mockResolvedValue({ active: false });
  mocks.migrateIssueCode.mockReset();
  mocks.identity = identity(true);
});

describe("Account passkey delete", () => {
  it("names the passkey in the confirmation and deletes only on confirm", async () => {
    mocks.passkeyList.mockResolvedValueOnce({ credentials: [laptop, phone] }).mockResolvedValue({ credentials: [phone] });
    mocks.passkeyDelete.mockResolvedValue(undefined);
    renderAccount();

    await userEvent.click(await screen.findByRole("button", deleteButton("Laptop")));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(t("account:passkey_delete_title"))).toBeTruthy();
    expect(within(dialog).getByText(/“Laptop”/)).toBeTruthy();
    expect(mocks.passkeyDelete).not.toHaveBeenCalled();

    await userEvent.click(within(dialog).getByRole("button", { name: t("account:passkey_delete_confirm") }));

    expect(mocks.passkeyDelete).toHaveBeenCalledWith("pk-1");
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(screen.queryByRole("button", deleteButton("Laptop"))).toBeNull());
    expect(screen.getByRole("button", deleteButton("Phone"))).toBeTruthy();
  });

  it("cancel leaves the passkey alone", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [laptop, phone] });
    renderAccount();

    await userEvent.click(await screen.findByRole("button", deleteButton("Laptop")));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: t("common:cancel") }));

    expect(mocks.passkeyDelete).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("locks the only passkey of an unverified account and says why", async () => {
    mocks.identity = identity(false);
    mocks.passkeyList.mockResolvedValue({ credentials: [laptop] });
    renderAccount();

    const button = await screen.findByRole("button", deleteButton("Laptop"));
    expect(button).toHaveProperty("disabled", true);
    expect(screen.getByText(t("account:passkey_last_hint"))).toBeTruthy();
  });

  it("lets a verified account delete its only passkey (email-OTP still signs it in)", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [laptop] });
    renderAccount();

    const button = await screen.findByRole("button", deleteButton("Laptop"));
    expect(button).toHaveProperty("disabled", false);
    expect(screen.queryByText(t("account:passkey_last_hint"))).toBeNull();
  });

  it("keeps the dialog open with the server's reason when the delete is refused", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [laptop, phone] });
    mocks.passkeyDelete.mockRejectedValue({ status: 409, code: "last_passkey", message: "raw" });
    renderAccount();

    await userEvent.click(await screen.findByRole("button", deleteButton("Laptop")));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: t("account:passkey_delete_confirm") }));

    const alert = await within(screen.getByRole("dialog")).findByRole("alert");
    expect(alert.textContent).toBe(t("errors:last_passkey"));
    expect(mocks.passkeyList).toHaveBeenCalledTimes(2);
  });

  it("treats a passkey already gone as deleted", async () => {
    mocks.passkeyList.mockResolvedValueOnce({ credentials: [laptop, phone] }).mockResolvedValue({ credentials: [phone] });
    mocks.passkeyDelete.mockRejectedValue({ status: 404, code: "not_found", message: "passkey not found" });
    renderAccount();

    await userEvent.click(await screen.findByRole("button", deleteButton("Laptop")));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: t("account:passkey_delete_confirm") }));

    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(screen.queryByRole("button", deleteButton("Laptop"))).toBeNull());
    expect(screen.getByRole("button", deleteButton("Phone"))).toBeTruthy();
    expect(screen.queryByText("passkey not found")).toBeNull();
  });

  it("refreshes the device list, since removing a passkey signs the other devices out", async () => {
    mocks.passkeyList.mockResolvedValueOnce({ credentials: [laptop, phone] }).mockResolvedValue({ credentials: [phone] });
    mocks.passkeyDelete.mockResolvedValue(undefined);
    mocks.listMySessions.mockReset();
    mocks.listMySessions.mockResolvedValueOnce([thisMac, otherPC]).mockResolvedValue([thisMac]);
    renderAccount();

    expect(await screen.findByText("Firefox on Windows")).toBeTruthy();
    await userEvent.click(await screen.findByRole("button", deleteButton("Laptop")));
    expect(within(screen.getByRole("dialog")).getByText(/Your other devices are signed out too\./)).toBeTruthy();
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: t("account:passkey_delete_confirm") }));

    await waitFor(() => expect(screen.queryByText("Firefox on Windows")).toBeNull());
    expect(screen.getByText("Chrome on macOS")).toBeTruthy();
    expect(mocks.listMySessions).toHaveBeenCalledTimes(2);
  });

  it("leaves the device list alone when the removal is refused", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [laptop, phone] });
    mocks.passkeyDelete.mockRejectedValue({ status: 409, code: "last_passkey", message: "raw" });
    renderAccount();

    await userEvent.click(await screen.findByRole("button", deleteButton("Laptop")));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: t("account:passkey_delete_confirm") }));
    await within(screen.getByRole("dialog")).findByRole("alert");

    expect(mocks.listMySessions).toHaveBeenCalledTimes(1);
  });
});

describe("Account migration", () => {
  it("shows until when the confirmation holds, and asks for it again once issuing is refused", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [] });
    const until = "2026-09-27T10:10:00Z";
    mocks.migrateStatus
      .mockResolvedValueOnce({ active: true, state: "confirmed", confirm_factor: "email_otp", confirm_expires_at: until })
      .mockResolvedValue({ active: true, state: "initiated" });
    mocks.migrateIssueCode.mockRejectedValue({ status: 409, code: "not_confirmed", message: "confirm first" });
    renderAccount();

    const deadline = t("account:migration_issue_deadline", { time: new Date(until).toLocaleTimeString() });
    expect(await screen.findByText(deadline)).toBeTruthy();
    await userEvent.type(screen.getByPlaceholderText(t("account:migration_target_placeholder")), "u-2");
    await userEvent.click(screen.getByRole("button", { name: t("account:migration_issue_btn") }));

    expect(mocks.migrateIssueCode).toHaveBeenCalledWith("u-2");
    expect(await screen.findByText(t("account:migration_confirm_title"))).toBeTruthy();
    expect(screen.getByText(t("errors:not_confirmed"))).toBeTruthy();
    expect(screen.queryByPlaceholderText(t("account:migration_target_placeholder"))).toBeNull();
  });
});
