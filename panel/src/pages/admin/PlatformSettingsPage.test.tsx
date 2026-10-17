// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { PlatformSettingsPage } from "./PlatformSettingsPage";
import type { WakePolicySettings } from "@/lib/types";
const calls = vi.hoisted(() => ({ getEntryPolicy: vi.fn(), setEntryPolicy: vi.fn(), fleet: vi.fn(), getWakePolicy: vi.fn(), setWakePolicy: vi.fn(), nodes: vi.fn(), nodeTasks: vi.fn(), nodeTask: vi.fn(), startNodeTask: vi.fn(), retryNodeTask: vi.fn(), getMailSettings: vi.fn(), getArchivePolicy: vi.fn(), getFailoverPolicy: vi.fn(), getHeartbeat: vi.fn(), hostOverview: vi.fn(), getSelfServiceQuota: vi.fn(), setSelfServiceQuota: vi.fn() }));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: calls }));
const runtime = vi.hoisted(() => ({ distributed: false, fallback: false, apiBase: "/api/v1", rootDomain: "example.test" }));
vi.mock("@/lib/hooks", async (original) => ({ ...await original<typeof import("@/lib/hooks")>(), useConfig: () => runtime }));
const policy: WakePolicySettings = { maxRunningServers: 0, wakeCooldownSeconds: 0, revision: "initial", managed: false };
const limit = () => screen.getByRole("spinbutton", { name: "Running server limit (0–10000)" }) as HTMLInputElement;
const cooldown = () => screen.getByRole("spinbutton", { name: "Wake cooldown (0–3600 seconds)" });
function page(tab?: string) { render(<MemoryRouter initialEntries={[tab ? `/admin/platform?tab=${tab}` : "/admin/platform"]}><PlatformSettingsPage /></MemoryRouter>); }
const overview = { host: "ctl-a", at: "2026-10-12T00:00:00Z", version: "v1", disks: [], units: [], proxy: {}, alerts: [], databaseBackups: [], offsite: { configured: false, keyKept: false },
  settings: { dbBackupTime: "03:30", dbBackupKeep: 14, velocityXmx: "1G" }, connection: { mode: "local", panelHost: "console.example.test", adminHost: "op.console.example.test" }, storage: { mode: "local" }, rootDomain: "example.test" };
beforeEach(() => {
  vi.clearAllMocks();
  runtime.distributed = false;
  runtime.fallback = false;
  calls.getEntryPolicy.mockResolvedValue({ mode: "domain", defaultServer: "", requireAccountLink: true, offlineAction: "wake", waitingSpace: "lobby", fallbackServer: "", revision: "initial" });
  calls.fleet.mockResolvedValue([]);
  calls.nodes.mockResolvedValue([]);
  calls.nodeTasks.mockResolvedValue({ available: false, tasks: [] });
  calls.getWakePolicy.mockResolvedValue(policy);
  calls.getMailSettings.mockResolvedValue({ source: "none", host: "", port: 587, from: "", username: "", password_set: false, require_tls: true, max_per_hour: 0, revision: "" });
  calls.getArchivePolicy.mockResolvedValue({ manualKeep: 5, manualRetentionDays: 30, manualCooldownMinutes: 10, scheduledEveryHours: 24, scheduledKeep: 7, scheduledRetentionDays: 90, releasedRetentionDays: 90, maxStoreGiB: 0, auditRetentionDays: 365, source: "host", revision: "" });
  calls.getHeartbeat.mockResolvedValue({ set: false, display: "", revision: "" });
  calls.getFailoverPolicy.mockResolvedValue({ automatic: false, afterMinutes: 15, replicaMinutes: 60, revision: "", managed: false });
  calls.hostOverview.mockResolvedValue(overview);
  calls.getSelfServiceQuota.mockResolvedValue({ enabled: false, max_servers: null, max_cpu_milli: null, max_memory_mb: null, max_storage_gb: null, revision: "" });
  calls.setWakePolicy.mockImplementation(async (body) => ({ ...body, revision: "saved", managed: true }));
});
describe("platform policy", () => {
  it("keeps the form visible during loading without offering placeholder values as saved configuration", async () => {
    let resolve!: (policy: WakePolicySettings) => void;
    calls.getWakePolicy.mockReturnValue(new Promise((r) => { resolve = r; }));
    page();
    expect(limit().disabled).toBe(true);
    expect(limit().value).toBe("");
    expect(screen.getAllByRole("status").some((node) => node.textContent?.includes("Loading the saved policy"))).toBe(true);
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

describe("page reload", () => {
  it("rereads every card from the header button and waits while a card holds unsaved changes", async () => {
    runtime.distributed = true;
    page();
    await waitFor(() => expect(limit().disabled).toBe(false));
    const lobby = screen.getByRole("radio", { name: /^Lobby/ });
    await waitFor(() => expect(lobby).toHaveProperty("disabled", false));
    const reload = screen.getByRole("button", { name: "Reload" });
    const reads = [calls.getWakePolicy, calls.getEntryPolicy, calls.fleet, calls.getMailSettings, calls.getArchivePolicy, calls.getHeartbeat, calls.getFailoverPolicy, calls.nodes, calls.nodeTasks, calls.hostOverview];
    await waitFor(() => { for (const read of reads) expect(read).toHaveBeenCalled(); });
    const before = reads.map((read) => read.mock.calls.length);
    await userEvent.click(reload);
    await waitFor(() => { reads.forEach((read, i) => expect(read.mock.calls.length).toBeGreaterThan(before[i])); });
    await userEvent.click(lobby);
    expect(reload).toHaveProperty("disabled", true);
    await userEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    expect(reload).toHaveProperty("disabled", false);
  });
});

describe("distributed deployment entry", () => {
  it("explains single-node deployment and enrollment without requesting unavailable nodes", async () => {
    page();
    await waitFor(() => expect(limit().disabled).toBe(false));
    expect(screen.getByText("Single-node mode")).toBeTruthy();
    expect(screen.getByText("Node operations")).toBeTruthy();
    expect((await screen.findAllByText(/Host node service is not installed/)).length).toBeGreaterThan(0);
    expect(calls.nodes).not.toHaveBeenCalled();
  });

  it("shows existing execution nodes and links to server placement and migration", async () => {
    runtime.distributed = true;
    calls.nodes.mockResolvedValue([{ name: "worker-b", role: "worker", ready: true, approved: false, architecture: "arm64", addresses: ["192.0.2.11"] }]);
    page("nodes");
    expect(await screen.findByText("worker-b")).toBeTruthy();
    expect(screen.getByText("Distributed mode enabled")).toBeTruthy();
    expect(screen.getByText("Pending approval")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Manage server placement and migration" }).getAttribute("href")).toBe("/servers");
  });

  it("keeps a failed config read distinct from single-node mode", async () => {
    runtime.fallback = true;
    runtime.distributed = true;
    page();
    await waitFor(() => expect(limit().disabled).toBe(false));
    expect(screen.getByText("Deployment mode unconfirmed")).toBeTruthy();
    expect(screen.queryByText("Single-node mode")).toBeNull();
    expect(calls.nodes).not.toHaveBeenCalled();
  });
});
