// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { UpdatesPage } from "./UpdatesPage";

const calls = vi.hoisted(() => ({
  getUpdateWindow: vi.fn(), getUpdateReport: vi.fn(), getDBBackup: vi.fn(), hostOverview: vi.fn(), nodeTasks: vi.fn(), fleet: vi.fn(),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

beforeEach(() => {
  vi.clearAllMocks();
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
