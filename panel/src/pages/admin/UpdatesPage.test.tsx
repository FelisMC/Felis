// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { UpdatesPage } from "./UpdatesPage";

const calls = vi.hoisted(() => ({
  getUpdateWindow: vi.fn(), getUpdateReport: vi.fn(), getDBBackup: vi.fn(), hostOverview: vi.fn(), nodeTasks: vi.fn(), fleet: vi.fn(),
  nodeTask: vi.fn(), startNodeTask: vi.fn(),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

beforeEach(() => {
  vi.clearAllMocks();
  calls.nodeTask.mockReset();
  calls.startNodeTask.mockReset();
  calls.getUpdateWindow.mockResolvedValue({ start: null, end: null });
  calls.getUpdateReport.mockResolvedValue({ report: null, max_age_seconds: 93600, stale: false });
  calls.getDBBackup.mockResolvedValue({ last: null, stale: true, max_age_seconds: 93600 });
  calls.nodeTasks.mockResolvedValue({ available: true, tasks: [] });
  calls.fleet.mockResolvedValue([]);
  calls.hostOverview.mockResolvedValue({
    host: "ctl-a", at: "2026-10-12T00:00:00Z", version: "v1", disks: [], units: [], proxy: {}, alerts: [], databaseBackups: [],
    offsite: { configured: false, keyKept: false }, settings: { dbBackupTime: "03:30", dbBackupKeep: 14, velocityXmx: "1G" },
    connection: { mode: "local", panelHost: "console.example.test", adminHost: "op.console.example.test" }, storage: { mode: "local" }, rootDomain: "example.test",
  });
});
afterEach(() => {
  vi.restoreAllMocks();
  return i18next.changeLanguage("en-US");
});

function page(path = "/admin/updates") {
  render(<MemoryRouter initialEntries={[path]}><UpdatesPage /></MemoryRouter>);
}

describe("UpdatesPage", () => {
  it("explains the maintenance guard and explicit apply flow on the update tab", async () => {
    page("/admin/updates?tab=updates");
    expect(
      await screen.findByText(
        "Apply is allowed inside this window. The host checks it before taking a backup and again before installation. Outside the window, apply is refused unless `--now` explicitly starts manual maintenance; `--force` does not bypass the checks.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("No maintenance window set. Configure one before applying, or explicitly use `--apply --now` for manual maintenance.")).toBeTruthy();
    expect(screen.queryByText(/applied automatically|may apply a Scheduled update/)).toBeNull();
    expect(screen.getByRole("tab", { name: "Updates" }).getAttribute("aria-selected")).toBe("true");
  });

  it("opens on the host's status and moves between tabs", async () => {
    page();
    expect(await screen.findByRole("heading", { name: "Host status" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Off-site copy" })).toBeNull();
    await userEvent.click(screen.getByRole("tab", { name: "Off-site copy" }));
    expect(screen.getByRole("heading", { name: "Off-site copy" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Host status" })).toBeNull();
  });

  it("rereads the host and every card from the header", async () => {
    page("/admin/updates?tab=backups");
    await waitFor(() => expect(calls.hostOverview).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByRole("button", { name: "Reload" })).toHaveProperty("disabled", false));
    const before = [calls.getDBBackup, calls.getUpdateReport, calls.getUpdateWindow, calls.nodeTasks].map((c) => c.mock.calls.length);
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(calls.hostOverview).toHaveBeenCalledTimes(2));
    await waitFor(() => [calls.getDBBackup, calls.getUpdateReport, calls.getUpdateWindow, calls.nodeTasks].forEach((c, i) => expect(c.mock.calls.length).toBeGreaterThan(before[i])));
  });
});

describe("UpdatesPage update now", () => {
  const t = (key: string, options: Record<string, string> = {}) => i18next.t(key, options);
  const component = (over: Record<string, unknown>) => ({ name: "felis-api", current: "v0.2.0", state: "available", selector: "panel", ...over });
  const reportOf = (...components: Record<string, unknown>[]) => ({
    report: { checked_at: new Date().toISOString(), felis: "v0.2.0", components },
    max_age_seconds: 93600,
    stale: false,
  });
  const updateTask = (over: Record<string, unknown>) => ({
    id: "u1",
    request: { action: "update", params: { version: "v0.3.0", now: "true" }, confirmMaintenance: true },
    actor: "owner",
    state: "running",
    stage: "update",
    startedAt: new Date().toISOString(),
    ...over,
  });
  const hours = (n: number) => new Date(Date.now() + n * 3_600_000).toISOString();
  const card = () => screen.findByRole("region", { name: t("admin:update_now_title", { version: "v0.3.0" }) });
  const updateButton = (scope: HTMLElement) => within(scope).getByRole("button", { name: t("admin:update_now_button") }) as HTMLButtonElement;

  beforeEach(() => {
    calls.getUpdateReport.mockResolvedValue(reportOf(component({ latest: "v0.3.0" })));
  });

  it("offers nothing unless the report holds an available Felis update", async () => {
    calls.getUpdateReport.mockResolvedValue(reportOf(component({ state: "current", current: "v0.3.0" }), component({ name: "k3s", latest: "v2", selector: "k3s" })));
    page("/admin/updates?tab=updates");

    await screen.findByText("k3s");
    expect(screen.queryByRole("button", { name: t("admin:update_now_button") })).toBeNull();
  });

  it("holds an update outside the maintenance window until that is confirmed too", async () => {
    calls.startNodeTask.mockResolvedValue(updateTask({ stage: "preflight" }));
    calls.nodeTask.mockResolvedValue(updateTask({ stage: "preflight" }));
    page("/admin/updates?tab=updates");
    const user = userEvent.setup();
    const section = await card();
    expect(within(section).getByText("v0.2.0")).toBeTruthy();
    await waitFor(() => expect(updateButton(section).disabled).toBe(false));
    await user.click(updateButton(section));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(t("admin:update_now_outside_window"))).toBeTruthy();
    const confirm = within(dialog).getByRole("button", { name: t("admin:update_now_confirm_btn") }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    await user.click(within(dialog).getByRole("checkbox", { name: t("admin:update_now_outside_confirm") }));
    expect(confirm.disabled).toBe(false);
    await user.click(confirm);

    expect(calls.startNodeTask).toHaveBeenCalledExactlyOnceWith({
      action: "update",
      params: { version: "v0.3.0", now: "true" },
      confirmMaintenance: true,
    });
  });

  it("updates inside the maintenance window on a plain confirmation", async () => {
    calls.getUpdateWindow.mockResolvedValue({ start: hours(-1), end: hours(1) });
    calls.startNodeTask.mockResolvedValue(updateTask({ stage: "preflight" }));
    calls.nodeTask.mockResolvedValue(updateTask({ stage: "preflight" }));
    page("/admin/updates?tab=updates");
    const user = userEvent.setup();
    const section = await card();
    await waitFor(() => expect(updateButton(section).disabled).toBe(false));
    await user.click(updateButton(section));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).queryByRole("checkbox")).toBeNull();
    await user.click(within(dialog).getByRole("button", { name: t("admin:update_now_confirm_btn") }));

    expect(calls.startNodeTask).toHaveBeenCalledExactlyOnceWith({
      action: "update",
      params: { version: "v0.3.0", now: "false" },
      confirmMaintenance: true,
    });
  });

  it("says the panel is restarting, not that an error occurred, while a running update cannot be read", async () => {
    const running = updateTask({});
    let restarting = false;
    calls.nodeTasks.mockResolvedValue({ available: true, tasks: [running] });
    calls.nodeTask.mockImplementation(async () => {
      if (restarting) throw { status: 502, code: "bad_gateway", message: "upstream unavailable" };
      return running;
    });
    page("/admin/updates?tab=updates");
    const section = await card();
    expect(await within(section).findByText(t("admin:host_stage_update"))).toBeTruthy();

    restarting = true;
    const reload = screen.getByRole("button", { name: t("admin:platform_reload") });
    await waitFor(() => expect((reload as HTMLButtonElement).disabled).toBe(false));
    await userEvent.click(reload);

    expect(await within(section).findByText(t("admin:update_now_reconnecting"))).toBeTruthy();
    expect(within(section).queryByRole("alert")).toBeNull();
    expect(within(section).queryByText(/upstream unavailable/)).toBeNull();
  });

  it("shows why an update failed and how to go back", async () => {
    const failed = updateTask({ state: "failed", error: "install script exited with status 1", log: "installer: checksum mismatch", finishedAt: new Date().toISOString() });
    calls.nodeTasks.mockResolvedValue({ available: true, tasks: [failed] });
    calls.nodeTask.mockResolvedValue(failed);
    page("/admin/updates?tab=updates");
    const section = await card();

    expect(await within(section).findByText("install script exited with status 1")).toBeTruthy();
    expect(within(section).getByText("installer: checksum mismatch")).toBeTruthy();
    expect(within(section).getByText(t("admin:update_now_rollback"))).toBeTruthy();
    expect(within(section).getByRole("link", { name: t("admin:update_now_rollback_link") }).getAttribute("href")).toBe(
      "https://docs.felismc.com/en/operations/upgrade#rollback",
    );
  });

  it("reports a finished update and asks for a reload while the report still lists it", async () => {
    const done = updateTask({ state: "succeeded", stage: "complete", finishedAt: new Date().toISOString() });
    calls.nodeTasks.mockResolvedValue({ available: true, tasks: [done] });
    calls.nodeTask.mockResolvedValue(done);
    page("/admin/updates?tab=updates");
    const section = await card();

    expect(await within(section).findByText(t("admin:update_now_done", { version: "v0.3.0" }))).toBeTruthy();
    expect(within(section).getByRole("button", { name: t("admin:update_now_reload") })).toBeTruthy();
    expect(within(section).queryByRole("button", { name: t("admin:update_now_button") })).toBeNull();
  });
});
