// @vitest-environment jsdom
import { useState } from "react";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { HostStatusCard } from "./HostStatusCard";
import { DatabaseBundlesCard } from "./DatabaseBundlesCard";
import { WorldBackupCard } from "./WorldBackupCard";
import { OffsiteCard, RestoreHostCard } from "./OffsiteCard";
import { AuditExportCard, DoctorCard } from "./DiagnosticsCards";
import { useHostTask } from "@/components/HostTask";
import type { AsyncState } from "@/lib/hooks";
import type { HostOverview, NodeControlTask } from "@/lib/types";

const calls = vi.hoisted(() => ({
  nodeTasks: vi.fn(), nodeTask: vi.fn(), startNodeTask: vi.fn(), holdAlerts: vi.fn(), revealOffsiteKey: vi.fn(), exportAudit: vi.fn(), fleet: vi.fn(),
}));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: calls }));
const saved = vi.hoisted(() => vi.fn());
vi.mock("@/lib/download", () => ({ saveBlob: saved }));

beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const base: HostOverview = {
  host: "ctl-a", at: "2026-10-12T00:00:00Z", version: "v1.4.0",
  disks: [{ path: "/var", total: 100 * 2 ** 30, available: 5 * 2 ** 30 }], memory: { total: 8 * 2 ** 30, available: 4 * 2 ** 30 },
  units: [{ name: "felis-velocity.service", active: "active" }, { name: "felis-db-backup.timer", active: "inactive" }],
  proxy: { address: "127.0.0.1:25565", reachable: true },
  alerts: [{ key: "disk/var", severity: "critical", summary: "磁盘将满", summary_en: "/var is 95% full", hint: "free space on /var", firstSeen: "2026-10-11T00:00:00Z" }],
  databaseBackups: [{ name: "felis-db-20261012T033000Z-daily.tar", label: "daily", size: 2048, created: "2026-10-12T03:30:00Z" }],
  offsite: { configured: false, keyKept: false },
  settings: { dbBackupTime: "03:30", dbBackupKeep: 14, velocityXmx: "1G" },
  connection: { mode: "local", panelHost: "console.example.test", adminHost: "op.console.example.test" },
  storage: { mode: "local" }, rootDomain: "example.test",
};
function state(data: HostOverview): AsyncState<HostOverview> {
  return { data, error: null, loading: false, updatedAt: 0, reload: vi.fn() };
}
const started = (action: NodeControlTask["request"]["action"]): NodeControlTask => ({ id: "t1", request: { action, confirmMaintenance: false }, actor: "owner", state: "running", stage: "preflight", startedAt: "2026-10-12T00:00:00Z" });

beforeEach(() => {
  vi.clearAllMocks();
  calls.nodeTasks.mockResolvedValue({ available: true, tasks: [] });
  calls.startNodeTask.mockImplementation(async (r) => ({ ...started(r.action), request: r }));
  calls.nodeTask.mockImplementation(async () => started("doctor"));
  calls.fleet.mockResolvedValue([{ name: "survival", displayName: "Survival" }, { name: "creative" }, { name: "lobby", system: true }]);
});

// finishInstantly makes every task the cards start report done, for flows that start several.
function finishInstantly() {
  calls.startNodeTask.mockImplementation(async (r) => ({ ...started(r.action), request: r, state: "succeeded", stage: "complete" }));
  calls.nodeTask.mockImplementation(async () => ({ ...started("doctor"), state: "succeeded", stage: "complete" }));
}

async function ready(name: string | RegExp) {
  await waitFor(() => expect(screen.getByRole("button", { name })).toHaveProperty("disabled", false));
  return screen.getByRole("button", { name });
}

describe("host status", () => {
  it("lists open alerts and stopped timers, and holds the watchdog's mail for the hours chosen", async () => {
    calls.holdAlerts.mockResolvedValue(null);
    const overview = state(base);
    render(<HostStatusCard overview={overview} />);
    expect(screen.getByText("1 open alert")).toBeTruthy();
    expect(screen.getByText("/var is 95% full")).toBeTruthy();
    expect(screen.getByText("1 service or timer needs attention")).toBeTruthy();
    expect(screen.getByRole("meter", { name: "/var" }).getAttribute("aria-valuenow")).toBe("95");
    await userEvent.click(screen.getByRole("button", { name: "Hold alert mail" }));
    await waitFor(() => expect(calls.holdAlerts).toHaveBeenCalledTimes(1));
    const until = new Date(calls.holdAlerts.mock.calls[0][0]).getTime();
    expect(Math.abs(until - (Date.now() + 4 * 3600_000))).toBeLessThan(60_000);
    expect(overview.reload).toHaveBeenCalled();
  });

  it("lifts a hold that is in place", async () => {
    calls.holdAlerts.mockResolvedValue(null);
    render(<HostStatusCard overview={state({ ...base, alerts: [], quietUntil: "2026-10-12T05:00:00Z" })} />);
    expect(screen.getByText("No open alerts")).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Resume alert mail" }));
    await waitFor(() => expect(calls.holdAlerts).toHaveBeenCalledWith(null));
  });
});

function Bundles({ overview }: { overview: AsyncState<HostOverview> }) {
  const db = useHostTask(["db-backup", "db-verify", "db-delete", "db-restore"]);
  return <DatabaseBundlesCard overview={overview} db={db} />;
}

describe("database backups", () => {
  it("verifies a bundle, and restores one only once its name is typed", async () => {
    finishInstantly();
    render(<Bundles overview={state(base)} />);
    await userEvent.click(await ready("Verify"));
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "db-verify", params: { bundle: "felis-db-20261012T033000Z-daily.tar" }, confirmMaintenance: false });
    await userEvent.click(await ready("Restore"));
    const dialog = await screen.findByRole("dialog");
    const confirm = within(dialog).getByRole("button", { name: "Restore" });
    expect(confirm).toHaveProperty("disabled", true);
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "felis-db-20261012T033000Z" } });
    expect(confirm).toHaveProperty("disabled", true);
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "felis-db-20261012T033000Z-daily.tar" } });
    await userEvent.click(confirm);
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenLastCalledWith({ action: "db-restore", params: { bundle: "felis-db-20261012T033000Z-daily.tar" }, confirmMaintenance: true }));
  });

  it("deletes an older bundle after confirmation, and the list no longer shows it", async () => {
    // A stand-in for the host: it holds the bundles, and only a db-delete task removes one.
    const older = "felis-db-20261011T090000Z-manual.tar";
    const host = { bundles: [...base.databaseBackups, { name: older, label: "manual", size: 1024, created: "2026-10-11T09:00:00Z" }] };
    let pending: string | null = null;
    calls.startNodeTask.mockImplementation(async (r: NodeControlTask["request"]) => {
      if (r.action === "db-delete") pending = r.params?.bundle ?? null;
      return { ...started(r.action), request: r };
    });
    calls.nodeTask.mockImplementation(async () => {
      host.bundles = host.bundles.filter((b) => b.name !== pending);
      return { ...started("db-delete"), state: "succeeded", stage: "complete" };
    });
    function Live() {
      const [data, setData] = useState<HostOverview>({ ...base, databaseBackups: host.bundles });
      const overview: AsyncState<HostOverview> = { data, error: null, loading: false, updatedAt: 0, reload: () => setData({ ...base, databaseBackups: [...host.bundles] }) };
      return <Bundles overview={overview} />;
    }
    render(<Live />);
    expect(screen.getByRole("button", { name: "Delete backup felis-db-20261012T033000Z-daily.tar" })).toHaveProperty("disabled", true);
    await userEvent.click(await ready(`Delete backup ${older}`));
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toContain(`${older} is deleted from this host`);
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(calls.startNodeTask).not.toHaveBeenCalled();
    await userEvent.click(await ready(`Delete backup ${older}`));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(screen.queryByText(older)).toBeNull());
    expect(screen.getByText("felis-db-20261012T033000Z-daily.tar")).toBeTruthy();
  });

  it("pages through more bundles than fit on one page", async () => {
    const day = (n: number) => String(n).padStart(2, "0");
    const many = Array.from({ length: 23 }, (_, i) => ({ name: `felis-db-202609${day(28 - i)}T033000Z-daily.tar`, label: "daily", size: 2048, created: `2026-09-${day(28 - i)}T03:30:00Z` }));
    render(<Bundles overview={state({ ...base, databaseBackups: many })} />);
    expect(screen.getAllByRole("button", { name: /^Delete backup / })).toHaveLength(10);
    expect(screen.getByText("Page 1 of 3")).toBeTruthy();
    expect(screen.getByText(many[0].name)).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Page 2 of 3")).toBeTruthy();
    expect(screen.queryByText(many[0].name)).toBeNull();
    expect(screen.getByText(many[10].name)).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getAllByRole("button", { name: /^Delete backup / })).toHaveLength(3);
  });

  it("saves only the schedule fields that changed", async () => {
    render(<Bundles overview={state(base)} />);
    const save = screen.getByRole("button", { name: "Save schedule" });
    expect(save).toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("Backup time"), { target: { value: "04:15" } });
    fireEvent.change(screen.getByLabelText("Daily backups kept"), { target: { value: "0" } });
    expect(save).toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("Daily backups kept"), { target: { value: "14" } });
    await waitFor(() => expect(save).toHaveProperty("disabled", false));
    await userEvent.click(save);
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "host-settings", params: { dbBackupTime: "04:15" }, confirmMaintenance: false });
  });
});

describe("world backups", () => {
  it("backs up every user server live by default", async () => {
    render(<WorldBackupCard />);
    await userEvent.click(await ready("Start backup"));
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "backup-now", params: {}, confirmMaintenance: false });
  });

  it("asks before stopping the chosen servers first", async () => {
    render(<WorldBackupCard />);
    await userEvent.click(screen.getByRole("radio", { name: "Selected servers" }));
    expect(await screen.findByText("Survival")).toBeTruthy();
    expect(screen.queryByText("lobby")).toBeNull();
    expect(screen.getByRole("button", { name: "Start backup" })).toHaveProperty("disabled", true);
    fireEvent.click(screen.getByRole("checkbox", { name: /Survival/ }));
    await userEvent.click(screen.getByRole("switch", { name: "Stop running servers first" }));
    await userEvent.click(await ready("Start backup"));
    const dialog = await screen.findByRole("dialog");
    expect(calls.startNodeTask).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Start backup" }));
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "backup-now", params: { stop: "true", servers: "survival" }, confirmMaintenance: true }));
  });
});

describe("off-site copy", () => {
  it("needs the bucket's keys the first time, and hands them over as secrets", async () => {
    render(<OffsiteCard overview={state(base)} />);
    expect(screen.getByText("No off-site bucket is configured. Every backup is stored only on this host.")).toBeTruthy();
    await userEvent.click(await ready("Configure"));
    fireEvent.change(screen.getByLabelText("Endpoint"), { target: { value: "https://s3.example.com" } });
    fireEvent.change(screen.getByLabelText("Bucket"), { target: { value: "felis-backup" } });
    const save = screen.getByRole("button", { name: "Save and check" });
    expect(save).toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("Access key ID"), { target: { value: "AKIA0" } });
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: "s3cret" } });
    await userEvent.click(save);
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "offsite-configure", params: { endpoint: "https://s3.example.com", bucket: "felis-backup" }, secrets: { accessKey: "AKIA0", secretKey: "s3cret" }, confirmMaintenance: false });
  });

  it("copies now, reveals the sealing key, and asks before turning the copy off", async () => {
    finishInstantly();
    calls.revealOffsiteKey.mockResolvedValue({ key: "c2VhbGluZy1rZXk=" });
    const overview = state({ ...base, offsite: { configured: true, endpoint: "https://s3.example.com", bucket: "felis-backup", keyKept: true, status: { last_success: "2026-10-12T01:00:00Z", last_attempt: "2026-10-12T01:00:00Z" } } });
    render(<OffsiteCard overview={overview} />);
    expect(screen.getByText("Copying normally")).toBeTruthy();
    await userEvent.click(await ready("Copy now"));
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "offsite-sync", confirmMaintenance: false });
    await userEvent.click(screen.getByRole("button", { name: "Show key" }));
    expect(await screen.findByText("c2VhbGluZy1rZXk=")).toBeTruthy();
    await userEvent.click(await ready("Turn off"));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Turn off" }));
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenLastCalledWith({ action: "offsite-disable", confirmMaintenance: true }));
  });

  it("rebuilds the host only once the word is typed", async () => {
    render(<RestoreHostCard overview={state({ ...base, offsite: { configured: true, bucket: "b", keyKept: true } })} />);
    fireEvent.change(screen.getByLabelText("Database backup (optional)"), { target: { value: "not-a-bundle" } });
    expect(screen.getByRole("button", { name: "Start rebuild" })).toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("Database backup (optional)"), { target: { value: "" } });
    await userEvent.click(await ready("Start rebuild"));
    const dialog = await screen.findByRole("dialog");
    const confirm = within(dialog).getByRole("button", { name: "Start rebuild" });
    expect(confirm).toHaveProperty("disabled", true);
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "REBUILD" } });
    await userEvent.click(confirm);
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "restore-host", params: { takeOver: "true", force: "false" }, confirmMaintenance: true }));
  });
});

describe("diagnostics", () => {
  it("shows a health check's findings by area", async () => {
    calls.nodeTasks.mockResolvedValue({ available: true, tasks: [{ ...started("doctor"), state: "succeeded" }] });
    calls.nodeTask.mockResolvedValue({ ...started("doctor"), state: "succeeded", stage: "complete", result: {
      host: "ctl-a", at: "2026-10-12T00:00:00Z", source: "checks run as felis-watchdog.service runs them", critical: 1, warning: 0,
      areas: [
        { key: "disk", title: "disk space", state: "problem", findings: [{ key: "disk/var", severity: "critical", summary: "磁盘将满", summary_en: "/var is 95% full", hint: "free space" }] },
        { key: "offsite", title: "off-site copy", state: "skipped", skipped: "not configured" },
        { key: "memory", title: "memory", state: "ok" },
      ] } });
    render(<DoctorCard />);
    expect(await screen.findByText("/var is 95% full")).toBeTruthy();
    expect(screen.getByText("1 critical, 0 warning")).toBeTruthy();
    expect(screen.getByText("Not checked: not configured")).toBeTruthy();
    expect(screen.getByText("Disk space")).toBeTruthy();
  });

  it("exports the audit log of a period", async () => {
    const blob = new Blob(["{}\n"]);
    calls.exportAudit.mockResolvedValue({ blob, filename: "felis-audit.jsonl" });
    render(<AuditExportCard />);
    fireEvent.change(screen.getByLabelText("From (inclusive)"), { target: { value: "2026-02-01" } });
    fireEvent.change(screen.getByLabelText("To (exclusive)"), { target: { value: "2026-01-01" } });
    expect(screen.getByRole("button", { name: "Download" })).toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("To (exclusive)"), { target: { value: "2026-03-01" } });
    await userEvent.click(screen.getByRole("button", { name: "Download" }));
    await waitFor(() => expect(saved).toHaveBeenCalledWith(blob, "felis-audit.jsonl"));
    expect(calls.exportAudit).toHaveBeenCalledWith("2026-02-01", "2026-03-01");
  });
});
