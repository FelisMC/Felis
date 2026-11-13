// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { Setup } from "./Setup";

const calls = vi.hoisted(() => ({
  setupRedeem: vi.fn(),
  setupStatus: vi.fn(),
  emailStart: vi.fn(),
  emailVerify: vi.fn(),
  passkeyRegisterBegin: vi.fn(),
  refresh: vi.fn(),
}));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({ loading: false, identity: null, refresh: calls.refresh }),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      setupRedeem: calls.setupRedeem,
      setupStatus: calls.setupStatus,
      emailStart: calls.emailStart,
      emailVerify: calls.emailVerify,
      passkeyRegisterBegin: calls.passkeyRegisterBegin,
    },
  };
});

const t = (key: string, opts?: Record<string, unknown>) => i18next.t(key, opts);

const fresh = {
  user_id: "o1",
  username: "owner",
  role: "owner",
  email: "",
  email_verified: false,
  has_passkey: false,
  setup_required: true,
  mail_available: false,
};

function renderSetup() {
  return render(
    <MemoryRouter initialEntries={["/setup?token=raw-token"]}>
      <Setup />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  // No session survives a failed redeem.
  calls.setupStatus.mockRejectedValue({ status: 401, code: "unauthenticated", message: "" });
});

describe("Setup", () => {
  it.each([
    [500, "internal"],
    [503, "service_unavailable"],
    [429, "rate_limited"],
    [0, "network_error"],
  ])("offers another try with the same link after a %i %s", async (status, code) => {
    calls.setupRedeem.mockRejectedValueOnce({ status, code, message: "" });
    calls.setupRedeem.mockResolvedValueOnce(fresh);
    renderSetup();

    expect(await screen.findByText(t("auth:setup_failed_title"))).toBeTruthy();
    expect(screen.queryByText(t("auth:setup_invalid_subtitle"))).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: t("auth:setup_retry") }));

    expect(await screen.findByText(t("auth:setup_welcome", { name: "owner" }))).toBeTruthy();
    expect(screen.getByText(t("auth:setup_game_optional"))).toBeTruthy();
    expect(calls.setupRedeem.mock.calls).toEqual([["raw-token"], ["raw-token"]]);
  });

  it("finishes with an email code when mail delivery works", async () => {
    calls.setupRedeem.mockResolvedValue({ ...fresh, mail_available: true });
    calls.emailStart.mockResolvedValue({ sent: true, expires_at: "" });
    calls.emailVerify.mockResolvedValue({ verified: true, email: "owner@example.net" });
    calls.setupStatus.mockResolvedValue({ ...fresh, mail_available: true, email: "owner@example.net", email_verified: true, setup_required: false });
    renderSetup();

    await userEvent.type(await screen.findByLabelText(t("auth:email_address")), "owner@example.net");
    await userEvent.click(screen.getByRole("button", { name: t("auth:setup_send_code") }));
    await userEvent.type(await screen.findByLabelText(t("auth:setup_code_label")), "123456");
    await userEvent.click(screen.getByRole("button", { name: t("auth:setup_verify_submit") }));

    await vi.waitFor(() => expect(calls.refresh).toHaveBeenCalled());
    expect(calls.emailStart).toHaveBeenCalledWith("owner@example.net");
    expect(calls.emailVerify).toHaveBeenCalledWith("123456");
  });

  it("explains the certificate when the browser refuses a passkey", async () => {
    calls.setupRedeem.mockResolvedValue({ ...fresh, email: "owner@example.net" });
    calls.passkeyRegisterBegin.mockResolvedValue({ challenge: "AAAA", user: { id: "AAAA", name: "owner", displayName: "owner" } });
    Object.defineProperty(navigator, "credentials", {
      configurable: true,
      value: { create: vi.fn().mockRejectedValue(new DOMException("blocked", "NotAllowedError")) },
    });
    renderSetup();

    await userEvent.click(await screen.findByRole("button", { name: t("auth:setup_create_passkey") }));
    expect(await screen.findByText(t("auth:setup_passkey_refused"))).toBeTruthy();
  });

  it("leads a refused passkey back to the email code when mail works", async () => {
    calls.setupRedeem.mockResolvedValue({ ...fresh, mail_available: true, email: "owner@example.net" });
    calls.passkeyRegisterBegin.mockResolvedValue({ challenge: "AAAA", user: { id: "AAAA", name: "owner", displayName: "owner" } });
    Object.defineProperty(navigator, "credentials", {
      configurable: true,
      value: { create: vi.fn().mockRejectedValue(new DOMException("WebAuthn is not supported on sites with TLS certificate errors.", "NotAllowedError")) },
    });
    renderSetup();

    await userEvent.click(await screen.findByRole("button", { name: t("auth:setup_use_passkey") }));
    await userEvent.click(await screen.findByRole("button", { name: t("auth:setup_create_passkey") }));
    expect(await screen.findByText(t("auth:setup_passkey_refused_mail"))).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: t("auth:setup_use_email") }));
    expect(await screen.findByRole("button", { name: t("auth:setup_send_code") })).toBeTruthy();
  });

  it("sends a spent link back to felis setup, with nothing to retry", async () => {
    calls.setupRedeem.mockRejectedValue({ status: 400, code: "setup_token_invalid", message: "" });
    renderSetup();

    expect(await screen.findByText(t("auth:setup_invalid_subtitle"))).toBeTruthy();
    expect(await screen.findByText(t("errors:setup_token_invalid"))).toBeTruthy();
    expect(screen.queryByRole("button", { name: t("auth:setup_retry") })).toBeNull();
  });
});
