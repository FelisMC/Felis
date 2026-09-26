// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { TierProvider, useTier } from "./tier";
import type { Identity } from "./types";

const calls = vi.hoisted(() => ({ me: vi.fn() }));
vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const admin: Identity = { user_id: "u1", email: "a@b.c", role: "admin", is_admin: true, is_owner: false };
const err503 = { status: 503, code: "unavailable", message: "restarting" };

let tier: ReturnType<typeof useTier>;
function Probe() {
  tier = useTier();
  return <p>{tier.isAdmin ? "admin" : "not admin"}</p>;
}

async function boot() {
  render(
    <TierProvider>
      <Probe />
    </TierProvider>,
  );
  await act(async () => {});
}

beforeEach(() => {
  calls.me.mockReset();
});

describe("TierProvider after a /me that failed with anything but a 401", () => {
  it("exposes the failure, and a revalidate that succeeds clears it without a reload", async () => {
    calls.me.mockRejectedValueOnce(err503);
    await boot();
    expect(tier.identityError).toBe(err503);
    expect(tier.unauthenticated).toBe(false);

    let answer!: (id: Identity) => void;
    calls.me.mockReturnValueOnce(new Promise<Identity>((resolve) => (answer = resolve)));
    let retry!: Promise<void>;
    act(() => {
      retry = tier.revalidate();
    });
    // Mid-check the app stays mounted: loading never comes back.
    expect(tier.loading).toBe(false);

    await act(async () => {
      answer(admin);
      await retry;
    });
    expect(tier.identityError).toBeNull();
    expect(screen.getByText("admin")).toBeTruthy();
  });

  it("keeps the failure when the retry fails the same way", async () => {
    calls.me.mockRejectedValueOnce(err503);
    await boot();

    calls.me.mockRejectedValueOnce(new TypeError("Failed to fetch"));
    await act(() => tier.revalidate());

    expect(tier.identityError).toBe(err503);
    expect(tier.unauthenticated).toBe(false);
  });
});
