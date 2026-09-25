// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { Login } from "./Login";

const calls = vi.hoisted(() => ({
  authEmailStart: vi.fn(),
  authEmailVerify: vi.fn(),
  authPasskeyDiscoverableBegin: vi.fn(),
  authPasskeyDiscoverableFinish: vi.fn(),
  authPasskeyLoginBegin: vi.fn(),
  authPasskeyLoginFinish: vi.fn(),
  credentialsGet: vi.fn(),
  refresh: vi.fn(),
}));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({ loading: false, identity: null, refresh: calls.refresh }),
}));
vi.mock("@/lib/config", () => ({
  loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "localhost" }),
  useConfig: () => null,
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      authEmailStart: calls.authEmailStart,
      authEmailVerify: calls.authEmailVerify,
      authPasskeyDiscoverableBegin: calls.authPasskeyDiscoverableBegin,
      authPasskeyDiscoverableFinish: calls.authPasskeyDiscoverableFinish,
      authPasskeyLoginBegin: calls.authPasskeyLoginBegin,
      authPasskeyLoginFinish: calls.authPasskeyLoginFinish,
    },
  };
});

const t = (key: string, opts?: Record<string, unknown>) => i18next.t(key, opts);

function renderLogin() {
  return render(
    <MemoryRouter initialEntries={["/login"]}>
      <Login />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  // jsdom has no WebAuthn; the browser handing back nothing is what a
  // dismissed or empty authenticator looks like to the page.
  Object.defineProperty(navigator, "credentials", { value: { get: calls.credentialsGet }, configurable: true });
});

describe("Login", () => {
  it("reads out why the code could not be sent", async () => {
    calls.authEmailStart.mockRejectedValue({ status: 429, code: "otp_resend_cooldown", message: "" });
    renderLogin();

    await userEvent.type(screen.getByLabelText(t("auth:email_address")), "a@b.c");
    await userEvent.click(screen.getByRole("button", { name: t("auth:send_otp") }));

    expect((await screen.findByRole("alert")).textContent).toBe(t("errors:otp_resend_cooldown"));
  });

  it("reads out a wrong code, and clears it on the next try", async () => {
    calls.authEmailStart.mockResolvedValue(undefined);
    calls.authEmailVerify.mockRejectedValueOnce({ status: 400, code: "invalid_code", message: "" });
    calls.authEmailVerify.mockReturnValueOnce(new Promise(() => {}));
    renderLogin();

    await userEvent.type(screen.getByLabelText(t("auth:email_address")), "a@b.c");
    await userEvent.click(screen.getByRole("button", { name: t("auth:send_otp") }));
    await userEvent.type(await screen.findByLabelText(t("auth:otp_code")), "000000");
    await userEvent.click(screen.getByRole("button", { name: t("auth:otp_btn") }));
    expect((await screen.findByRole("alert")).textContent).toBe(t("errors:invalid_code"));

    await userEvent.click(screen.getByRole("button", { name: t("auth:otp_btn") }));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("words an empty passkey answer in the UI language, with and without an email", async () => {
    calls.credentialsGet.mockResolvedValue(null);
    calls.authPasskeyDiscoverableBegin.mockResolvedValue({ login_id: "l1", publicKey: { challenge: "AAAA" } });
    calls.authPasskeyLoginBegin.mockResolvedValue({ challenge: "AAAA" });
    renderLogin();

    await userEvent.click(screen.getByRole("button", { name: t("auth:passkey_btn") }));
    expect((await screen.findByRole("alert")).textContent).toBe("The browser returned no passkey. Try again.");
    expect(calls.authPasskeyDiscoverableFinish).not.toHaveBeenCalled();

    await userEvent.type(screen.getByLabelText(t("auth:email_address")), "a@b.c");
    await userEvent.click(screen.getByRole("button", { name: t("auth:passkey_btn") }));
    await vi.waitFor(() => expect(calls.authPasskeyLoginBegin).toHaveBeenCalledWith("a@b.c"));
    expect((await screen.findByRole("alert")).textContent).toBe("The browser returned no passkey. Try again.");
    expect(calls.authPasskeyLoginFinish).not.toHaveBeenCalled();
  });
});
