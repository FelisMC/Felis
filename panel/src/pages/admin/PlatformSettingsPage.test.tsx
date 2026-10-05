// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { PlatformSettingsPage } from "./PlatformSettingsPage";
import type { WakePolicySettings } from "@/lib/types";
const calls = vi.hoisted(() => ({ getWakePolicy: vi.fn(), setWakePolicy: vi.fn() }));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: calls }));
const policy: WakePolicySettings = { maxRunningServers: 0, wakeCooldownSeconds: 0, revision: "initial", managed: false };
const limit = () => screen.getByRole("spinbutton", { name: "Running server limit (0–10000)" }) as HTMLInputElement;
const cooldown = () => screen.getByRole("spinbutton", { name: "Wake cooldown (0–3600 seconds)" });
function page() { render(<MemoryRouter><PlatformSettingsPage /></MemoryRouter>); }
beforeEach(() => {
  vi.clearAllMocks();
  calls.getWakePolicy.mockResolvedValue(policy);
  calls.setWakePolicy.mockImplementation(async (body) => ({ ...body, revision: "saved", managed: true }));
});
describe("platform policy", () => {
  it("keeps the form visible during loading without offering placeholder values as saved configuration", async () => {
    let resolve!: (policy: WakePolicySettings) => void;
    calls.getWakePolicy.mockReturnValue(new Promise((r) => { resolve = r; }));
    page();
    expect(limit().disabled).toBe(true);
    expect(limit().value).toBe("");
    expect(screen.getByRole("status").textContent).toContain("Loading the saved policy");
    await act(async () => resolve(policy));
    expect(limit().disabled).toBe(false);
    expect(limit().value).toBe("0");
  });
  it("saves validated values with the read revision and preserves drafts on a conflict", async () => {
    page(); await waitFor(() => expect(limit().disabled).toBe(false));
    fireEvent.change(limit(), { target: { value: "2" } });
    fireEvent.change(cooldown(), { target: { value: "30" } });
    calls.setWakePolicy.mockRejectedValueOnce({ status: 409, code: "conflict", message: "changed" });
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await screen.findByRole("alert");
    expect(limit().value).toBe("2");
    expect(screen.getByRole("button", { name: "Reload" })).toHaveProperty("disabled", true);
    await userEvent.click(screen.getByRole("button", { name: "Save and apply" }));
    await screen.findByText("Saved. The new policy is active without a restart.");
    expect(calls.setWakePolicy).toHaveBeenLastCalledWith({ maxRunningServers: 2, wakeCooldownSeconds: 30, revision: "initial" });
    fireEvent.change(limit(), { target: { value: "1.5" } });
    expect(screen.getByRole("button", { name: "Save and apply" })).toHaveProperty("disabled", true);
    await userEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    expect(limit().value).toBe("2");
  });
});
