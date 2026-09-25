// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
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
  opLoginStart: vi.fn(),
  opLoginStatus: vi.fn(),
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
      opLoginStart: calls.opLoginStart,
      opLoginStatus: calls.opLoginStatus,
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

// The status endpoint says approved:false for a dead request too, so the page ends
// the wait itself: at start's expires_at, or when the server refuses outright.
describe("operator sign-in", () => {
  const WAITING = () => screen.getByText(t("auth:op_waiting"));

  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(new Date("2026-09-25T12:00:00Z"));
  });
  afterEach(() => vi.useRealTimers());

  async function startOp(expiresInMs: number) {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    calls.opLoginStart.mockImplementation(async () => ({
      request_id: `req-${calls.opLoginStart.mock.calls.length}`,
      expires_at: new Date(Date.now() + expiresInMs).toISOString(),
    }));
    renderLogin();
    await user.click(screen.getByRole("button", { name: t("auth:tab_op_btn") }));
    await user.type(screen.getByLabelText(t("auth:email_address")), "op@example.test");
    await user.click(screen.getByRole("button", { name: t("auth:op_start_btn") }));
    await screen.findByText("/felis web op approve req-1");
    return user;
  }

  it("stops asking at the deadline and offers a new request", async () => {
    calls.opLoginStatus.mockResolvedValue({ approved: false });
    const user = await startOp(10_000);
    expect(screen.getByText(t("auth:op_expires_in", { time: "0:10" }))).toBeTruthy();

    await vi.advanceTimersByTimeAsync(9_500);
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(3);
    expect(WAITING()).toBeTruthy();

    await vi.advanceTimersByTimeAsync(1_000);
    expect((await screen.findByRole("alert")).textContent).toBe(t("auth:op_expired"));
    expect((screen.getByLabelText(t("auth:otp_code")) as HTMLInputElement).disabled).toBe(true);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(3);

    await user.click(screen.getByRole("button", { name: t("auth:op_request_again") }));
    await screen.findByText("/felis web op approve req-2");
    expect(calls.opLoginStart).toHaveBeenLastCalledWith("op@example.test");
    expect(screen.queryByRole("alert")).toBeNull();
    await vi.advanceTimersByTimeAsync(3_200);
    expect(calls.opLoginStatus).toHaveBeenLastCalledWith("req-2");
  });

  it("backs off while status calls fail and returns to the normal pace after one succeeds", async () => {
    const offline = { status: 0, code: "network_error", message: "" };
    calls.opLoginStatus
      .mockRejectedValueOnce(offline)
      .mockRejectedValueOnce(offline)
      .mockRejectedValueOnce(offline)
      .mockResolvedValue({ approved: false });
    await startOp(300_000);

    await vi.advanceTimersByTimeAsync(3_200); // t=3s: first failure, next in 6s
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(1);
    expect(screen.getByText(t("auth:op_poll_retrying"))).toBeTruthy();
    await vi.advanceTimersByTimeAsync(5_500); // t=8.7s
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(600); // t=9.3s: second failure, next in 12s
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(11_400); // t=20.7s
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(600); // t=21.3s: third failure, next in 24s
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(23_400); // t=44.7s
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(600); // t=45.3s: success, next in 3s
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(4);
    expect(WAITING()).toBeTruthy();
    await vi.advanceTimersByTimeAsync(3_000);
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(5);
  });

  it("stops and says why when the server refuses outright", async () => {
    calls.opLoginStatus.mockRejectedValue({ status: 403, code: "local_auth_disabled", message: "" });
    await startOp(300_000);

    await vi.advanceTimersByTimeAsync(3_200);
    expect((await screen.findByRole("alert")).textContent).toBe(t("errors:local_auth_disabled"));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(1);
  });

  it("stops asking once approved and keeps the countdown for the code", async () => {
    calls.opLoginStatus.mockResolvedValueOnce({ approved: false }).mockResolvedValue({ approved: true });
    await startOp(60_000);

    await vi.advanceTimersByTimeAsync(6_200);
    expect(screen.getByText(t("auth:op_approved"))).toBeTruthy();
    expect(screen.getByText(t("auth:op_expires_in", { time: "0:54" }))).toBeTruthy();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(calls.opLoginStatus).toHaveBeenCalledTimes(2);
  });
});

