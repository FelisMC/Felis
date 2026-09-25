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
    api: { ...actual.api, authEmailStart: calls.authEmailStart, authEmailVerify: calls.authEmailVerify },
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
});
