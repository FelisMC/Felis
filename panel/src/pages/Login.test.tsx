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
  authOwnerStatus: vi.fn(),
  credentialsGet: vi.fn(),
  refresh: vi.fn(),
}));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({ loading: false, identity: null, refresh: calls.refresh }),
}));
// The page builds the join address with the real helpers; only the file is faked.
const config = vi.hoisted(() => ({ value: {} as Record<string, unknown> }));
vi.mock("@/lib/config", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/config")>();
  return { ...actual, loadConfig: () => Promise.resolve(config.value), useConfig: () => null };
});
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
      authOwnerStatus: calls.authOwnerStatus,
    },
  };
});

const t = (key: string, opts?: Record<string, unknown>) => i18next.t(key, opts);

// renderLogin waits out the Owner probe, which holds the page on a spinner, and
// returns once the heading is the one wanted: the doors by default.
async function renderLogin(heading = t("auth:login_title")) {
  const view = render(
    <MemoryRouter initialEntries={["/login"]}>
      <Login />
    </MemoryRouter>,
  );
  await screen.findByRole("heading", { name: heading });
  return view;
}

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  calls.authOwnerStatus.mockResolvedValue({ owner_bound: true });
  config.value = { apiBase: "/api/v1", rootDomain: "localhost" };
  // jsdom has no WebAuthn; the browser handing back nothing is what a
  // dismissed or empty authenticator looks like to the page.
  Object.defineProperty(navigator, "credentials", { value: { get: calls.credentialsGet }, configurable: true });
});

describe("Login", () => {
  it("starts with login methods on the operator host after signing out", async () => {
    config.value.adminHostname = window.location.hostname;
    await renderLogin();
    expect(screen.getByRole("button", { name: t("auth:passkey_btn") })).toBeTruthy();
    expect(screen.getByRole("button", { name: t("auth:tab_op_btn") })).toBeTruthy();
    expect(screen.queryByRole("button", { name: t("auth:op_start_btn") })).toBeNull();
  });

  it("reads out why the code could not be sent", async () => {
    calls.authEmailStart.mockRejectedValue({ status: 429, code: "otp_resend_cooldown", message: "" });
    await renderLogin();

    await userEvent.type(screen.getByLabelText(t("auth:email_address")), "a@b.c");
    await userEvent.click(screen.getByRole("button", { name: t("auth:send_otp") }));

    expect((await screen.findByRole("alert")).textContent).toBe(t("errors:otp_resend_cooldown"));
  });

  it("reads out a wrong code, and clears it on the next try", async () => {
    calls.authEmailStart.mockResolvedValue(undefined);
    calls.authEmailVerify.mockRejectedValueOnce({ status: 400, code: "invalid_code", message: "" });
    calls.authEmailVerify.mockReturnValueOnce(new Promise(() => {}));
    await renderLogin();

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
    await renderLogin();

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
    await renderLogin();
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


// Host setup creates the Owner before any game identity is linked.
describe("an install with no Owner", () => {
  const NO_OWNER = () => t("auth:no_owner_title");

  beforeEach(() => {
    calls.authOwnerStatus.mockResolvedValue({ owner_bound: false });
  });

  it("guides the administrator from host setup to the browser without entering Minecraft", async () => {
    config.value = { apiBase: "/api/v1", rootDomain: "203.0.113.7.nip.io", gamePort: 25570 };
    await renderLogin(NO_OWNER());

    expect(screen.getByText("sudo felis setup")).toBeTruthy();
    expect(screen.getByText(t("auth:no_owner_step_link"))).toBeTruthy();
    expect(screen.queryByText("203.0.113.7:25570")).toBeNull();
    expect(screen.queryByText("/link")).toBeNull();
    expect(screen.queryByLabelText(t("auth:email_address"))).toBeNull();
    expect(screen.queryByRole("button", { name: t("auth:passkey_btn") })).toBeNull();
    expect(screen.queryByRole("button", { name: t("auth:tab_bind_btn") })).toBeNull();
  });

  it("checks again on request and shows the doors once an Owner is bound", async () => {
    calls.authOwnerStatus
      .mockResolvedValueOnce({ owner_bound: false })
      .mockResolvedValueOnce({ owner_bound: false })
      .mockResolvedValue({ owner_bound: true });
    await renderLogin(NO_OWNER());

    await userEvent.click(screen.getByRole("button", { name: t("auth:no_owner_recheck") }));
    expect(await screen.findByText(t("auth:no_owner_still_unbound"))).toBeTruthy();
    expect(calls.authOwnerStatus).toHaveBeenCalledTimes(2);

    await userEvent.click(screen.getByRole("button", { name: t("auth:no_owner_recheck") }));
    await screen.findByRole("heading", { name: t("auth:login_title") });
    expect(screen.getByLabelText(t("auth:email_address"))).toBeTruthy();
  });

  it("holds the page while it asks, so an unclaimed install never flashes the doors", async () => {
    calls.authOwnerStatus.mockReturnValue(new Promise(() => {}));
    render(
      <MemoryRouter initialEntries={["/login"]}>
        <Login />
      </MemoryRouter>,
    );
    await vi.waitFor(() => expect(calls.authOwnerStatus).toHaveBeenCalled());

    expect(screen.getByText(t("common:loading"))).toBeTruthy();
    expect(screen.queryByLabelText(t("auth:email_address"))).toBeNull();
  });

  it("shows the doors when the server cannot say", async () => {
    calls.authOwnerStatus.mockRejectedValue({ status: 503, code: "auth_unavailable", message: "" });
    await renderLogin();

    expect(screen.getByLabelText(t("auth:email_address"))).toBeTruthy();
  });
});

// A player gets a bind code by joining in Minecraft, so the hint names where.
describe("the bind-code door", () => {
  it("names the address to join", async () => {
    config.value = { apiBase: "/api/v1", rootDomain: "mc.example", gamePort: 25570 };
    await renderLogin();
    await userEvent.click(screen.getByRole("button", { name: t("auth:tab_bind_btn") }));

    expect(screen.getByText(t("auth:bind_hint"))).toBeTruthy();
    expect(screen.getByText("mc.example:25570")).toBeTruthy();
  });

  it("names the server when config.json could not be read", async () => {
    config.value = { apiBase: "/api/v1", rootDomain: "localhost", fallback: true };
    await renderLogin();
    await userEvent.click(screen.getByRole("button", { name: t("auth:tab_bind_btn") }));

    expect(screen.getByText(t("auth:bind_hint_no_address"))).toBeTruthy();
    expect(screen.queryByText("localhost")).toBeNull();
  });
});
