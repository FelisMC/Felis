// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { Identity, PasskeyCredential } from "@/lib/types";
import { Account } from "./Account";

// The Account page's changes to how the account signs in (delete or add a
// passkey, change the email) meet 403 reauth_required when the session has not
// proven a factor lately. These drive the page through that refusal: the check
// dialog, each factor, and the change running again afterwards.

const mocks = vi.hoisted(() => ({
  passkeyList: vi.fn(),
  passkeyDelete: vi.fn(),
  passkeyRegisterBegin: vi.fn(),
  listMySessions: vi.fn(),
  emailStart: vi.fn(),
  emailVerify: vi.fn(),
  reauthStatus: vi.fn(),
  reauthPasskeyBegin: vi.fn(),
  reauthPasskeyFinish: vi.fn(),
  reauthEmailStart: vi.fn(),
  reauthEmailVerify: vi.fn(),
  logout: vi.fn(),
  refresh: vi.fn(),
  credentialsGet: vi.fn(),
  credentialsCreate: vi.fn(),
  identity: null as Identity | null,
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      linkStatus: () => Promise.resolve({ linked: true }),
      migrateStatus: () => Promise.resolve({ active: false }),
      passkeyList: mocks.passkeyList,
      passkeyDelete: mocks.passkeyDelete,
      passkeyRegisterBegin: mocks.passkeyRegisterBegin,
      listMySessions: mocks.listMySessions,
      emailStart: mocks.emailStart,
      emailVerify: mocks.emailVerify,
      reauthStatus: mocks.reauthStatus,
      reauthPasskeyBegin: mocks.reauthPasskeyBegin,
      reauthPasskeyFinish: mocks.reauthPasskeyFinish,
      reauthEmailStart: mocks.reauthEmailStart,
      reauthEmailVerify: mocks.reauthEmailVerify,
      logout: mocks.logout,
    },
  };
});
vi.mock("@/lib/tier", () => ({
  useTier: () => ({ identity: mocks.identity, refresh: mocks.refresh }),
}));

const refused = { status: 403, code: "reauth_required", message: "confirm it's you first" };
const laptop: PasskeyCredential = { id: "pk-1", name: "Laptop", created_at: "2026-03-01T10:00:00Z" };
const phone: PasskeyCredential = { id: "pk-2", name: "Phone", created_at: "2026-04-01T10:00:00Z" };

function identity(role: Identity["role"] = "user"): Identity {
  return {
    user_id: "u-1",
    email: "a@example.com",
    role,
    is_admin: role !== "user",
    is_owner: role === "owner",
    email_verified: true,
  };
}

const bytes = (s: string) => new TextEncoder().encode(s).buffer;

function renderAccount() {
  return render(
    <MemoryRouter>
      <Account />
    </MemoryRouter>,
  );
}

const checkDialog = () => screen.findByRole("dialog", { name: "Confirm it's you" });

async function askToDeleteLaptop() {
  await userEvent.click(await screen.findByRole("button", { name: "Delete passkey “Laptop”" }));
  const confirmDelete = screen.getByRole("dialog", { name: "Delete this passkey?" });
  await userEvent.click(within(confirmDelete).getByRole("button", { name: "Delete" }));
}

async function proveByEmail(dialog: HTMLElement) {
  await userEvent.click(await within(dialog).findByRole("button", { name: "Email a code to a@example.com" }));
  await userEvent.type(await within(dialog).findByLabelText("Code sent to a@example.com"), "123456");
  await userEvent.click(within(dialog).getByRole("button", { name: "Confirm" }));
}

beforeEach(() => {
  for (const fn of Object.values(mocks)) if (typeof fn === "function") fn.mockReset();
  mocks.identity = identity();
  mocks.listMySessions.mockResolvedValue([]);
  mocks.reauthEmailStart.mockResolvedValue({ sent: true, expires_at: "2026-09-25T10:10:00Z" });
  mocks.reauthEmailVerify.mockResolvedValue({ ok: true, until: "2026-09-25T10:05:00Z" });
  mocks.reauthPasskeyFinish.mockResolvedValue({ ok: true, until: "2026-09-25T10:05:00Z" });
  Object.defineProperty(navigator, "credentials", {
    value: { get: mocks.credentialsGet, create: mocks.credentialsCreate },
    configurable: true,
  });
});

describe("Account re-authentication", () => {
  it("asks for an email code before deleting a passkey, then deletes it", async () => {
    mocks.passkeyList.mockResolvedValueOnce({ credentials: [laptop, phone] }).mockResolvedValue({ credentials: [phone] });
    mocks.passkeyDelete.mockRejectedValueOnce(refused).mockResolvedValue(undefined);
    mocks.reauthStatus.mockResolvedValue({ needed: true, factors: ["email"] });
    renderAccount();

    await askToDeleteLaptop();
    const dialog = await checkDialog();
    expect(within(dialog).queryByRole("button", { name: "Use a passkey" })).toBeNull();
    await proveByEmail(dialog);

    expect(mocks.reauthEmailStart).toHaveBeenCalledTimes(1);
    expect(mocks.reauthEmailVerify).toHaveBeenCalledWith("123456");
    await waitFor(() => expect(mocks.passkeyDelete).toHaveBeenCalledTimes(2));
    expect(mocks.passkeyDelete).toHaveBeenLastCalledWith("pk-1");
    await waitFor(() => expect(screen.queryByRole("button", { name: "Delete passkey “Laptop”" })).toBeNull());
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("closing the check keeps the passkey and the delete dialog, with no error", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [laptop, phone] });
    mocks.passkeyDelete.mockRejectedValue(refused);
    mocks.reauthStatus.mockResolvedValue({ needed: true, factors: ["email"] });
    renderAccount();

    await askToDeleteLaptop();
    const dialog = await checkDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Confirm it's you" })).toBeNull());
    const confirmDelete = screen.getByRole("dialog", { name: "Delete this passkey?" });
    expect(within(confirmDelete).queryByRole("alert")).toBeNull();
    expect(mocks.passkeyDelete).toHaveBeenCalledTimes(1);
    expect(mocks.passkeyList).toHaveBeenCalledTimes(1);
  });

  it("a wrong code keeps the check open with the reason and changes nothing", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [laptop, phone] });
    mocks.passkeyDelete.mockRejectedValue(refused);
    mocks.reauthStatus.mockResolvedValue({ needed: true, factors: ["email"] });
    mocks.reauthEmailVerify.mockRejectedValue({ status: 400, code: "invalid_code", message: "raw" });
    renderAccount();

    await askToDeleteLaptop();
    const dialog = await checkDialog();
    await proveByEmail(dialog);

    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("That code is invalid or expired — request a fresh one and try again.");
    expect(mocks.passkeyDelete).toHaveBeenCalledTimes(1);
  });

  it("proves with a passkey from the envelope the begin returns", async () => {
    mocks.passkeyList.mockResolvedValueOnce({ credentials: [laptop, phone] }).mockResolvedValue({ credentials: [phone] });
    mocks.passkeyDelete.mockRejectedValueOnce(refused).mockResolvedValue(undefined);
    mocks.reauthStatus.mockResolvedValue({ needed: true, factors: ["passkey", "email"] });
    mocks.reauthPasskeyBegin.mockResolvedValue({
      publicKey: { challenge: "Y2hhbGxlbmdl", allowCredentials: [{ type: "public-key", id: "cGstMQ" }] },
    });
    mocks.credentialsGet.mockResolvedValue({
      id: "cred-1",
      rawId: bytes("raw"),
      type: "public-key",
      response: {
        clientDataJSON: bytes("cd"),
        authenticatorData: bytes("ad"),
        signature: bytes("sig"),
        userHandle: null,
      },
    });
    renderAccount();

    await askToDeleteLaptop();
    const dialog = await checkDialog();
    // Both factors are offered, the passkey first.
    const buttons = within(dialog).getAllByRole("button").map((b) => b.textContent);
    expect(buttons.indexOf("Use a passkey")).toBeLessThan(buttons.indexOf("Email a code to a@example.com"));
    await userEvent.click(within(dialog).getByRole("button", { name: "Use a passkey" }));

    await waitFor(() => expect(mocks.passkeyDelete).toHaveBeenCalledTimes(2));
    const publicKey = mocks.credentialsGet.mock.calls[0][0].publicKey;
    expect(new TextDecoder().decode(publicKey.challenge)).toBe("challenge");
    expect(new TextDecoder().decode(publicKey.allowCredentials[0].id)).toBe("pk-1");
    expect(mocks.reauthPasskeyFinish).toHaveBeenCalledWith({
      id: "cred-1",
      rawId: "cmF3",
      type: "public-key",
      response: { clientDataJSON: "Y2Q", authenticatorData: "YWQ", signature: "c2ln", userHandle: null },
    });
    expect(mocks.reauthEmailStart).not.toHaveBeenCalled();
  });

  it("offers operators a fresh sign-in instead of an email code", async () => {
    mocks.identity = identity("admin");
    mocks.passkeyList.mockResolvedValue({ credentials: [laptop, phone] });
    mocks.passkeyDelete.mockRejectedValue(refused);
    mocks.reauthStatus.mockResolvedValue({ needed: true, factors: ["sign_in"] });
    mocks.logout.mockResolvedValue(undefined);
    renderAccount();

    await askToDeleteLaptop();
    const dialog = await checkDialog();
    expect(
      within(dialog).getByText("Operator accounts can also sign out and sign in again. A fresh sign-in counts for 5 minutes."),
    ).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: /Email a code/ })).toBeNull();
    await userEvent.click(within(dialog).getByRole("button", { name: "Sign out and sign in again" }));

    expect(mocks.logout).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(mocks.refresh).toHaveBeenCalled());
  });

  it("goes straight on when the session was proven meanwhile", async () => {
    mocks.passkeyList.mockResolvedValueOnce({ credentials: [laptop, phone] }).mockResolvedValue({ credentials: [phone] });
    mocks.passkeyDelete.mockRejectedValueOnce(refused).mockResolvedValue(undefined);
    mocks.reauthStatus.mockResolvedValue({ needed: false, until: "2026-09-25T10:05:00Z", factors: ["email"] });
    renderAccount();

    await askToDeleteLaptop();

    await waitFor(() => expect(mocks.passkeyDelete).toHaveBeenCalledTimes(2));
    expect(mocks.reauthEmailStart).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("after the check, adding a passkey waits for Continue instead of starting the browser prompt", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [laptop] });
    mocks.passkeyRegisterBegin.mockRejectedValue(refused);
    mocks.reauthStatus.mockResolvedValue({ needed: true, factors: ["email"] });
    renderAccount();

    await userEvent.click(await screen.findByRole("button", { name: "Add Passkey" }));
    const register = screen.getByRole("dialog", { name: "Add Passkey" });
    await userEvent.type(within(register).getByLabelText("Device Nickname"), "Phone");
    await userEvent.click(within(register).getByRole("button", { name: "Continue" }));
    await proveByEmail(await checkDialog());

    const status = await within(screen.getByRole("dialog", { name: "Add Passkey" })).findByRole("status");
    expect(status.textContent).toBe("Confirmed. Press Continue to add the passkey.");
    expect(mocks.passkeyRegisterBegin).toHaveBeenCalledTimes(1);
    expect(mocks.credentialsCreate).not.toHaveBeenCalled();
    expect(within(screen.getByRole("dialog", { name: "Add Passkey" })).getByRole("button", { name: "Continue" })).toHaveProperty(
      "disabled",
      false,
    );
  });

  it("changes a verified email after the check and refreshes the devices it signs out", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [] });
    mocks.emailStart.mockRejectedValueOnce(refused).mockResolvedValue({ sent: true, expires_at: "2026-09-25T10:10:00Z" });
    mocks.emailVerify.mockResolvedValue({ verified: true, email: "new@example.com" });
    mocks.reauthStatus.mockResolvedValue({ needed: true, factors: ["email"] });
    renderAccount();

    await userEvent.click(await screen.findByRole("button", { name: "Change email" }));
    expect(
      screen.getByText(
        "Enter the new address and we'll send it a code. Once it's verified, sign-in codes go there, the old address gets a notice, and your other devices are signed out.",
      ),
    ).toBeTruthy();
    const address = screen.getByPlaceholderText("user@example.com");
    expect((address as HTMLInputElement).value).toBe("");
    await userEvent.type(address, "new@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Send Code" }));
    await proveByEmail(await checkDialog());

    await waitFor(() => expect(mocks.emailStart).toHaveBeenCalledTimes(2));
    expect(mocks.emailStart).toHaveBeenLastCalledWith("new@example.com");
    expect(await screen.findByText(/Verification code sent\./)).toBeTruthy();

    await userEvent.type(screen.getByPlaceholderText("6-digit code"), "654321");
    await userEvent.click(screen.getByRole("button", { name: "Verify" }));

    expect(mocks.emailVerify).toHaveBeenCalledWith("654321");
    await waitFor(() => expect(mocks.listMySessions).toHaveBeenCalledTimes(2));
    expect(mocks.refresh).toHaveBeenCalled();
  });

  it("cancelling a change of email returns to the verified address", async () => {
    mocks.passkeyList.mockResolvedValue({ credentials: [] });
    renderAccount();

    await userEvent.click(await screen.findByRole("button", { name: "Change email" }));
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.getByText("a@example.com")).toBeTruthy();
    expect(screen.queryByPlaceholderText("user@example.com")).toBeNull();
    expect(mocks.emailStart).not.toHaveBeenCalled();
  });
});
