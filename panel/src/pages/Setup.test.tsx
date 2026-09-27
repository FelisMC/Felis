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
  refresh: vi.fn(),
}));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({ loading: false, identity: null, refresh: calls.refresh }),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: { ...actual.api, setupRedeem: calls.setupRedeem, setupStatus: calls.setupStatus },
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
    expect(calls.setupRedeem.mock.calls).toEqual([["raw-token"], ["raw-token"]]);
  });

  it("sends a spent link back to felis setup, with nothing to retry", async () => {
    calls.setupRedeem.mockRejectedValue({ status: 400, code: "setup_token_invalid", message: "" });
    renderSetup();

    expect(await screen.findByText(t("auth:setup_invalid_subtitle"))).toBeTruthy();
    expect(await screen.findByText(t("errors:setup_token_invalid"))).toBeTruthy();
    expect(screen.queryByRole("button", { name: t("auth:setup_retry") })).toBeNull();
  });
});
