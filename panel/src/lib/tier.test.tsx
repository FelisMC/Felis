// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { ACCESS_RECHECK_MS, TierProvider, useTier } from "./tier";
import { ACCESS_REFUSED_EVENT } from "./api";
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
afterEach(() => {
  vi.useRealTimers();
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

// An admin demoted while the page is open keeps the admin pages up until /me is
// read again; the first refused call does that, at most once per interval.
describe("TierProvider after a role refusal", () => {
  const demoted: Identity = { ...admin, role: "user", is_admin: false };
  const refuse = () =>
    act(async () => {
      window.dispatchEvent(new Event(ACCESS_REFUSED_EVENT));
    });

  it("re-reads /me and takes the admin view down once the account was demoted", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.me.mockResolvedValue(admin);
    await boot();
    expect(screen.getByText("admin")).toBeTruthy();

    calls.me.mockResolvedValue(demoted);
    await act(() => vi.advanceTimersByTimeAsync(ACCESS_RECHECK_MS));
    await refuse();
    expect(await screen.findByText("not admin")).toBeTruthy();
    expect(calls.me).toHaveBeenCalledTimes(2);
  });

  it("re-reads at most once per interval", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.me.mockResolvedValue(admin);
    await boot();

    await act(() => vi.advanceTimersByTimeAsync(ACCESS_RECHECK_MS / 2));
    await refuse();
    expect(calls.me).toHaveBeenCalledTimes(1);

    await act(() => vi.advanceTimersByTimeAsync(ACCESS_RECHECK_MS / 2));
    await refuse();
    await refuse();
    expect(calls.me).toHaveBeenCalledTimes(2);
    expect(screen.getByText("admin")).toBeTruthy();
  });
});
