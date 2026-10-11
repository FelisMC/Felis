// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RetentionCard } from "./RetentionCard";
import type { ArchivePolicySettings } from "@/lib/types";

const calls = vi.hoisted(() => ({ getArchivePolicy: vi.fn(), setArchivePolicy: vi.fn(), clearArchivePolicy: vi.fn() }));
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const host: ArchivePolicySettings = {
  manualKeep: 5, manualRetentionDays: 30, manualCooldownMinutes: 10, scheduledEveryHours: 24, scheduledKeep: 7, scheduledRetentionDays: 90,
  releasedRetentionDays: 90, maxStoreGiB: 0, auditRetentionDays: 365, source: "host", revision: "rev-0",
};
const { source: _source, revision: _revision, ...policy } = host;

beforeEach(() => {
  vi.clearAllMocks();
  calls.getArchivePolicy.mockResolvedValue(host);
});

async function ready(manualKeep = "5") {
  const user = userEvent.setup();
  render(<RetentionCard />);
  const keep = (await screen.findAllByLabelText("Kept per server"))[0] as HTMLInputElement;
  await waitFor(() => expect(keep.value).toBe(manualKeep));
  return { user, keep, save: screen.getByRole("button", { name: "Save retention settings" }) as HTMLButtonElement };
}

describe("retention settings", () => {
  it("shows felis.toml's values until the Owner saves, and saves every field with the revision", async () => {
    calls.setArchivePolicy.mockImplementation(async (p) => ({ ...p, source: "panel", revision: "rev-1" }));
    const { user, keep, save } = await ready();
    expect(screen.getByText("Install-time settings")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Restore install-time settings" })).toBeNull();
    expect(save.disabled).toBe(true);
    await user.clear(keep);
    await user.type(keep, "9");
    const audit = screen.getByLabelText("Days audit rows are kept");
    await user.clear(audit);
    await user.type(audit, "0");
    expect(save.disabled).toBe(false);
    await user.click(save);
    await waitFor(() => expect(calls.setArchivePolicy).toHaveBeenCalledWith({ ...policy, manualKeep: 9, auditRetentionDays: 0, revision: "rev-0" }));
    expect(await screen.findByText("Retention settings saved.")).toBeTruthy();
    expect(screen.getByText("Panel settings")).toBeTruthy();
  });

  it("refuses a value outside its bounds and marks the field", async () => {
    const { user, save } = await ready();
    for (const [label, value] of [["Days audit rows are kept", "29"], ["Interval (hours)", "721"], ["Minutes between two backups", "1441"]] as const) {
      const field = screen.getByLabelText(label);
      await user.clear(field);
      await user.type(field, value);
      expect(save.disabled).toBe(true);
      expect(field.getAttribute("aria-invalid")).toBe("true");
      await user.clear(field);
      await user.type(field, "0");
      expect(field.getAttribute("aria-invalid")).toBe("false");
    }
    expect(save.disabled).toBe(false);
  });

  it("hands the retention back to felis.toml after confirming", async () => {
    calls.getArchivePolicy.mockResolvedValue({ ...host, manualKeep: 9, source: "panel", revision: "rev-1" });
    calls.clearArchivePolicy.mockResolvedValue(host);
    const { user, keep } = await ready("9");
    await user.click(screen.getByRole("button", { name: "Restore install-time settings" }));
    expect(calls.clearArchivePolicy).not.toHaveBeenCalled();
    await user.click(screen.getAllByRole("button", { name: "Restore install-time settings" }).at(-1)!);
    await waitFor(() => expect(calls.clearArchivePolicy).toHaveBeenCalled());
    expect(await screen.findByText("Install-time settings restored.")).toBeTruthy();
    expect(keep.value).toBe("5");
  });
});
