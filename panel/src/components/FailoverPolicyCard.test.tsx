// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FailoverPolicyCard } from "./FailoverPolicyCard";
import type { FailoverPolicySettings } from "@/lib/types";

const calls = vi.hoisted(() => ({ getFailoverPolicy: vi.fn(), setFailoverPolicy: vi.fn() }));
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const initial: FailoverPolicySettings = { automatic: false, afterMinutes: 15, replicaMinutes: 60, revision: "rev-0", managed: false };

beforeEach(() => {
  vi.clearAllMocks();
  calls.getFailoverPolicy.mockResolvedValue(initial);
});

async function ready() {
  const user = userEvent.setup();
  render(<FailoverPolicyCard />);
  const replica = (await screen.findByLabelText("World replica interval (minutes)")) as HTMLInputElement;
  await waitFor(() => expect(replica.value).toBe("60"));
  return { user, replica, after: screen.getByLabelText("Minutes lost before an automatic failover") as HTMLInputElement, save: screen.getByRole("button", { name: "Save failover setting" }) as HTMLButtonElement };
}

describe("failover setting", () => {
  it("starts with the Owner confirming, and the wait only applies once automatic failover is on", async () => {
    const { user, after, save } = await ready();
    expect(screen.getByText("Owner confirms")).toBeTruthy();
    expect(after.disabled).toBe(true);
    expect(save.disabled).toBe(true);
    await user.click(screen.getByRole("switch", { name: /Automatic failover/ }));
    expect(after.disabled).toBe(false);
    expect(save.disabled).toBe(false);
  });

  it("refuses values outside the bounds before saving", async () => {
    const { user, replica, after, save } = await ready();
    await user.click(screen.getByRole("switch", { name: /Automatic failover/ }));
    for (const [field, value] of [[after, "9"], [after, "1441"], [replica, "14"], [replica, "1441"]] as const) {
      await user.clear(field);
      await user.type(field, value);
      expect(save.disabled).toBe(true);
      expect(screen.getByText(/The automatic failover waits 10 to 1440 minutes/)).toBeTruthy();
      await user.clear(field);
      await user.type(field, field === after ? "15" : "60");
    }
    await user.clear(replica);
    await user.type(replica, "0");
    expect(save.disabled).toBe(false);
  });

  it("saves the setting with its revision and shows the saved state", async () => {
    calls.setFailoverPolicy.mockResolvedValue({ automatic: true, afterMinutes: 30, replicaMinutes: 120, revision: "rev-1", managed: true });
    const { user, replica, after, save } = await ready();
    await user.click(screen.getByRole("switch", { name: /Automatic failover/ }));
    await user.clear(after);
    await user.type(after, "30");
    await user.clear(replica);
    await user.type(replica, "120");
    await user.click(save);
    await waitFor(() => expect(calls.setFailoverPolicy).toHaveBeenCalledWith({ automatic: true, afterMinutes: 30, replicaMinutes: 120, revision: "rev-0" }));
    expect(await screen.findByText("Failover setting saved.")).toBeTruthy();
    expect(screen.getByText("Automatic")).toBeTruthy();
    expect(save.disabled).toBe(true);
  });
});
