// @vitest-environment jsdom
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MailSettingsCard } from "./MailSettingsCard";
import type { MailSettings } from "@/lib/types";

const calls = vi.hoisted(() => ({ getMailSettings: vi.fn(), setMailSettings: vi.fn(), clearMailSettings: vi.fn(), testMailSettings: vi.fn() }));
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

// Radix Select opens through pointer capture, which jsdom lacks.
beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const host: MailSettings = { source: "host", host: "smtp.example.com", port: 587, from: "noreply@example.com", username: "noreply@example.com", password_set: true, require_tls: true, max_per_hour: 0, revision: "" };
const panel: MailSettings = { ...host, source: "panel", host: "mail.example.org", revision: "rev-1" };

beforeEach(() => {
  vi.clearAllMocks();
  calls.getMailSettings.mockResolvedValue(host);
});

async function ready() {
  const user = userEvent.setup();
  render(<MailSettingsCard />);
  const field = (await screen.findByLabelText("Server host")) as HTMLInputElement;
  await waitFor(() => expect(field.value).toBe("smtp.example.com"));
  return user;
}

describe("mail settings", () => {
  it("shows where the relay comes from and keeps the stored password unless one is typed", async () => {
    const user = await ready();
    expect(screen.getByText("Install-time settings")).toBeTruthy();
    expect((screen.getByLabelText("Password") as HTMLInputElement).placeholder).toBe("Saved; leave empty to keep it");
    const save = screen.getByRole("button", { name: "Check and save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);

    calls.setMailSettings.mockResolvedValue(panel);
    await user.clear(screen.getByLabelText("Server host"));
    await user.type(screen.getByLabelText("Server host"), "mail.example.org");
    expect(screen.getByText("You have unsaved changes")).toBeTruthy();
    await user.click(save);
    await waitFor(() => expect(calls.setMailSettings).toHaveBeenCalledTimes(1));
    const sent = calls.setMailSettings.mock.calls[0][0];
    expect(sent).toEqual({ host: "mail.example.org", port: 587, from: "noreply@example.com", username: "noreply@example.com", require_tls: true, max_per_hour: 0, revision: "" });
    expect("password" in sent).toBe(false);
    expect(await screen.findByText("Mail delivery is saved and in effect.")).toBeTruthy();
    expect(screen.getByText("Panel settings")).toBeTruthy();
  });

  it("sends a typed password and a chosen port", async () => {
    const user = await ready();
    calls.setMailSettings.mockResolvedValue(panel);
    await user.type(screen.getByLabelText("Password"), "app-password");
    await user.click(screen.getByRole("combobox", { name: "Port" }));
    await user.click(await screen.findByRole("option", { name: "465 (SSL/TLS)" }));
    await user.click(screen.getByRole("button", { name: "Check and save" }));
    await waitFor(() => expect(calls.setMailSettings).toHaveBeenCalled());
    expect(calls.setMailSettings.mock.calls[0][0]).toMatchObject({ password: "app-password", port: 465 });
  });

  it("explains a relay that refused the check, in the panel's words", async () => {
    const user = await ready();
    calls.setMailSettings.mockRejectedValue({ status: 400, code: "smtp_auth", message: "the relay refused the user name or password" });
    await user.type(screen.getByLabelText("Password"), "wrong");
    await user.click(screen.getByRole("button", { name: "Check and save" }));
    expect(await screen.findByText("The mail server refused the user name or password.")).toBeTruthy();
    expect(screen.getByText("Install-time settings")).toBeTruthy();
  });

  it("mails a test message without saving", async () => {
    const user = await ready();
    calls.testMailSettings.mockResolvedValue({ sent_to: "owner@example.net" });
    await user.click(screen.getByRole("button", { name: "Send test message" }));
    expect(await screen.findByText("A test message was sent to owner@example.net.")).toBeTruthy();
    expect(calls.setMailSettings).not.toHaveBeenCalled();
  });

  it("reverts to the install-time relay after confirming", async () => {
    calls.getMailSettings.mockResolvedValue(panel);
    const user = userEvent.setup();
    render(<MailSettingsCard />);
    await user.click(await screen.findByRole("button", { name: "Revert to install-time settings" }));
    expect(calls.clearMailSettings).not.toHaveBeenCalled();
    expect(screen.getByText(/Felis will use the relay felis setup wrote at install time/)).toBeTruthy();
    calls.clearMailSettings.mockResolvedValue(host);
    await user.click(screen.getByRole("button", { name: "Revert to install-time settings" }));
    await waitFor(() => expect(calls.clearMailSettings).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Reverted to the install-time settings.")).toBeTruthy();
    expect(screen.getByText("Install-time settings")).toBeTruthy();
  });

  it("asks for a relay where none is configured", async () => {
    calls.getMailSettings.mockResolvedValue({ ...host, source: "none", host: "", from: "", username: "", password_set: false });
    render(<MailSettingsCard />);
    expect(await screen.findByText("Not configured")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Revert to install-time settings" })).toBeNull();
    expect((screen.getByRole("button", { name: "Send test message" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
