// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { ServerBackups } from "./ServerBackups";
import { humanizeError } from "@/lib/api";
import type { BackupView } from "@/lib/types";

const calls = vi.hoisted(() => ({
  status: vi.fn(),
  myServers: vi.fn(),
  serverJobs: vi.fn(),
  listBackups: vi.fn(),
  stop: vi.fn(),
  restoreBackup: vi.fn(),
  deleteBackup: vi.fn(),
  startBackupExport: vi.fn(),
  startWorldExport: vi.fn(),
  exportStatus: vi.fn(),
  exportDownloadURL: vi.fn(),
}));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({
    loading: false,
    identity: { user_id: "admin-1", email: "admin@example.test", role: "admin" },
    isAdmin: true,
    isOwner: false,
  }),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

function backup(id: string, hoursAgo: number, corrupt = false): BackupView {
  return {
    id,
    server_name: "survival",
    former_owner: "user-1",
    size_bytes: 1024 * 1024,
    reason: "manual",
    status: "present",
    created_at: new Date(Date.now() - hoursAgo * 3600_000).toISOString(),
    expires_at: new Date(Date.now() + 7 * 24 * 3600_000).toISOString(),
    corrupt,
  };
}

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  calls.status.mockResolvedValue({ name: "survival", displayName: "Survival", phase: "Stopped" });
  calls.myServers.mockResolvedValue([]);
  calls.serverJobs.mockResolvedValue([]);
  // 45 backups of this server; the API hands them over 20 at a time.
  calls.listBackups.mockImplementation(async ({ offset }: { offset: number }) => ({
    backups: offset === 0 ? [backup("bk-new", 1, true), backup("bk-ok", 2)] : [backup(`bk-${offset}`, 50)],
    total: 45,
  }));
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  return i18next.changeLanguage("en-US");
});

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/servers/survival/backups"]}>
      <Routes>
        <Route path="/servers/:name/backups" element={<ServerBackups />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("ServerBackups", () => {
  it("reads only this server's backups a page at a time, and marks the latest on the first page", async () => {
    renderPage();
    expect(await screen.findByText("Page 1 of 3")).toBeTruthy();
    expect(calls.listBackups).toHaveBeenLastCalledWith({ server: "survival", limit: 20, offset: 0 });
    expect(screen.getAllByText("Latest backup")).toHaveLength(1);
    // The newest row failed its read-back; the latest restorable one is the next.
    expect(screen.getByText("Latest backup").closest("tr")?.textContent).toContain("2 hours ago");

    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    expect(await screen.findByText("Page 2 of 3")).toBeTruthy();
    expect(calls.listBackups).toHaveBeenLastCalledWith({ server: "survival", limit: 20, offset: 20 });
    expect(screen.queryByText("Latest backup")).toBeNull();
  });

  it("names the daily restore points felis-api takes, in the list and in recent operations", async () => {
    calls.listBackups.mockResolvedValue({
      backups: [{ ...backup("bk-sched", 3), reason: "scheduled" }, backup("bk-manual", 5)],
      total: 2,
    });
    calls.serverJobs.mockResolvedValue([
      { name: "backup-survival-aa", kind: "backup", state: "succeeded", scheduled: true },
      { name: "backup-survival-bb", kind: "backup", state: "succeeded" },
    ]);
    renderPage();
    const sched = await screen.findByText("3 hours ago");
    expect(sched.closest("tr")?.textContent).toContain("Scheduled backup");
    expect(screen.getByText("5 hours ago").closest("tr")?.textContent).toContain("Manual backup");
    const ops = await screen.findAllByText(/^(Scheduled backup|Backup)$/, { selector: "li span" });
    expect(ops.map((el) => el.textContent)).toEqual(["Scheduled backup", "Backup"]);
  });

  it("names each kind of recent operation, with a download icon on the downloads", async () => {
    calls.serverJobs.mockResolvedValue([
      { name: "restore-survival-aa", kind: "restore", state: "succeeded" },
      { name: "backup-survival-bb", kind: "backup", state: "succeeded", then_restore: "done" },
      { name: "export-survival-cc", kind: "export_world", state: "succeeded" },
      { name: "export-survival-dd", kind: "export_backup", state: "failed" },
      { name: "files-survival-ee", kind: "export_files", state: "succeeded" },
    ]);
    renderPage();
    await screen.findByText("File download", { selector: "li span" });

    const rows = screen.getAllByRole("listitem").filter((li) => li.querySelector("span.text-sm"));
    expect(rows.map((li) => [li.querySelector("span.text-sm")?.textContent, li.querySelector("svg")?.classList.contains("lucide-download")])).toEqual([
      ["Restore", false],
      ["Pre-restore snapshot", false],
      ["World export", true],
      ["Backup download", true],
      ["File download", true],
    ]);
  });

  it("names the archive the reaper leaves when a server is given up or deleted", async () => {
    calls.listBackups.mockResolvedValue({ backups: [{ ...backup("bk-rel", 4), reason: "released" }], total: 1 });
    renderPage();
    expect((await screen.findByText("4 hours ago")).closest("tr")?.textContent).toContain("Archived when given up or deleted");
  });

  it("shows no pager when the server's backups fit on one page", async () => {
    calls.listBackups.mockResolvedValue({ backups: [backup("bk-1", 3)], total: 1 });
    renderPage();
    expect(await screen.findByText("Latest backup")).toBeTruthy();
    expect(screen.queryByText(/^Page \d+ of/)).toBeNull();
  });

  // Restoring a running server stops it first and restores only once it is down.
  // With players online the operator warns them for 30 s before the stop, and the
  // saves follow, so the wait must outlast that.
  async function restoreRunningServer(stopsAfterMs: number) {
    let stoppedAt = Infinity;
    calls.status.mockImplementation(async () => {
      const since = Date.now() - stoppedAt;
      // Running through the warning, Stopping while the pod saves and goes.
      const phase = since >= stopsAfterMs ? "Stopped" : since >= 30_000 ? "Stopping" : "Running";
      return { name: "survival", displayName: "Survival", phase };
    });
    calls.stop.mockImplementation(async () => {
      stoppedAt = Date.now();
    });
    calls.restoreBackup.mockResolvedValue({ safety_snapshot: true });
    calls.listBackups.mockResolvedValue({ backups: [backup("bk-1", 3)], total: 1 });
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Restore" }));
    const confirm = await screen.findByRole("button", { name: "Confirm restore" });
    vi.useFakeTimers();
    await act(async () => {
      fireEvent.click(confirm);
    });
    expect(calls.stop).toHaveBeenCalledWith("survival");
    await act(() => vi.advanceTimersByTimeAsync(130_000));
  }

  it("waits out a stop held for the players' 30-second warning, then restores", async () => {
    await restoreRunningServer(75_000);
    expect(calls.restoreBackup).toHaveBeenCalledWith("survival", "bk-1", true);
    expect(screen.queryByText(i18next.t("backups:stop_timeout"))).toBeNull();
  });

  it("gives up without restoring when the server never goes down", async () => {
    await restoreRunningServer(Infinity);
    expect(calls.restoreBackup).not.toHaveBeenCalled();
    expect(screen.getByText(i18next.t("backups:stop_timeout"))).toBeTruthy();
  });
});

describe("ServerBackups back up now", () => {
  const backUp = () => screen.getByRole("button", { name: "Back up now" }) as HTMLButtonElement;

  it("follows the server, so a stop lands on the page without a reload", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.status.mockResolvedValue({ name: "survival", displayName: "Survival", phase: "Running", desiredState: "Running" });
    renderPage();
    await screen.findByText("Page 1 of 3");
    expect(backUp().disabled).toBe(true);
    expect(backUp().title).toBe("Stop the server before backing it up.");

    calls.status.mockResolvedValue({ name: "survival", displayName: "Survival", phase: "Stopped", desiredState: "Stopped" });
    await act(() => vi.advanceTimersByTimeAsync(14_000));
    expect(backUp().disabled).toBe(true);
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    expect(backUp().disabled).toBe(false);
    expect(backUp().title).toBe("");
  });

  it("rereads fast while the server is on its way somewhere", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.status.mockResolvedValue({ name: "survival", displayName: "Survival", phase: "Stopping", desiredState: "Stopped" });
    renderPage();
    await screen.findByText("Page 1 of 3");
    const before = calls.status.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(4_000));
    expect(calls.status.mock.calls.length).toBe(before + 1);
  });

  it("counts a server just woken as starting and offers no backup", async () => {
    calls.status.mockResolvedValue({ name: "survival", displayName: "Survival", phase: "Stopped", desiredState: "Running" });
    renderPage();
    await screen.findByText("Page 1 of 3");
    expect(backUp().disabled).toBe(true);
    expect(screen.getByText("Starting")).toBeTruthy();
  });

  it.each([
    ["a backup running", { name: "backup-survival-aa", kind: "backup", state: "running" }],
    // The safety snapshot is done and the restore it leads to has yet to start.
    ["a restore about to start", { name: "backup-survival-bb", kind: "backup", state: "succeeded", then_restore: "pending" }],
  ])("waits for %s", async (_, job) => {
    calls.serverJobs.mockResolvedValue([job]);
    renderPage();
    await screen.findByText("Page 1 of 3");
    await vi.waitFor(() => expect(backUp().disabled).toBe(true));
    expect(backUp().title).toBe("A backup, restoration, or world export is in progress. Wait for the current operation to complete before starting a backup.");
  });

  it("keeps the page when a reread fails, and says the status may be stale", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderPage();
    await screen.findByText("Page 1 of 3");
    const outage = { status: 503, code: "unavailable", message: "status unavailable" };
    calls.status.mockRejectedValue(outage);
    await act(() => vi.advanceTimersByTimeAsync(15_000));
    expect((await screen.findByRole("alert")).textContent).toContain(humanizeError(outage));
    expect(screen.getByText("Page 1 of 3")).toBeTruthy();
    expect(backUp().disabled).toBe(false);
  });
});

describe("ServerBackups delete", () => {
  const row = (when: string) => screen.getByText(when).closest("tr") as HTMLElement;
  const deleteIn = (when: string) =>
    within(row(when)).getByRole("button", { name: "Delete backup" }) as HTMLButtonElement;
  const archiveWarning = () => screen.queryByText(i18next.t("backups:delete_archive_warning"));
  const onlyWarning = () => screen.queryByText(i18next.t("backups:delete_only_warning"));

  it("deletes the picked backup after the confirm, then rereads the list", async () => {
    calls.listBackups
      .mockResolvedValueOnce({ backups: [backup("bk-new", 1), backup("bk-ok", 2)], total: 2 })
      .mockResolvedValue({ backups: [backup("bk-new", 1)], total: 1 });
    calls.deleteBackup.mockResolvedValue({ id: "bk-ok", status: "expired" });
    renderPage();
    await screen.findByText("2 hours ago");
    await userEvent.click(deleteIn("2 hours ago"));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Delete this backup?")).toBeTruthy();
    expect(dialog.textContent).toContain("1.0 MiB");
    expect(dialog.textContent).toContain(i18next.t("backups:delete_cleanup_note"));
    expect(archiveWarning()).toBeNull();
    expect(onlyWarning()).toBeNull();
    expect(calls.deleteBackup).not.toHaveBeenCalled();

    await userEvent.click(within(dialog).getByRole("button", { name: "Delete backup" }));
    expect(calls.deleteBackup).toHaveBeenCalledExactlyOnceWith("bk-ok");
    expect(await screen.findByText("Backup deleted.")).toBeTruthy();
    await vi.waitFor(() => expect(screen.queryByText("2 hours ago")).toBeNull());
    expect(calls.listBackups).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it.each(["inactive_15d", "released"])("warns that a %s archive may be the world's only copy", async (reason) => {
    calls.listBackups.mockResolvedValue({
      backups: [{ ...backup("bk-arc", 4), reason }, backup("bk-man", 5)],
      total: 2,
    });
    renderPage();
    await screen.findByText("4 hours ago");
    await userEvent.click(deleteIn("4 hours ago"));
    await screen.findByRole("dialog");
    expect(archiveWarning()).toBeTruthy();
    expect(onlyWarning()).toBeNull();
  });

  it("says when it is the server's only backup", async () => {
    calls.listBackups.mockResolvedValue({ backups: [backup("bk-1", 3)], total: 1 });
    renderPage();
    await screen.findByText("3 hours ago");
    await userEvent.click(deleteIn("3 hours ago"));
    await screen.findByRole("dialog");
    expect(onlyWarning()).toBeTruthy();
    expect(archiveWarning()).toBeNull();
  });

  it("keeps the dialog open and says why when a restore may be reading it", async () => {
    calls.listBackups.mockResolvedValue({ backups: [backup("bk-1", 3), backup("bk-2", 4)], total: 2 });
    calls.deleteBackup.mockRejectedValue({ status: 409, code: "restore_in_progress", message: "busy" });
    renderPage();
    await screen.findByText("3 hours ago");
    await userEvent.click(deleteIn("3 hours ago"));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete backup" }));
    expect(await within(dialog).findByText(i18next.t("backups:delete_restore_busy"))).toBeTruthy();
    expect(screen.getByRole("dialog")).toBe(dialog);
    expect(calls.listBackups).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("Backup deleted.")).toBeNull();
  });

  it("rereads the list when the backup was already gone", async () => {
    calls.listBackups
      .mockResolvedValueOnce({ backups: [backup("bk-1", 3), backup("bk-2", 4)], total: 2 })
      .mockResolvedValue({ backups: [backup("bk-2", 4)], total: 1 });
    calls.deleteBackup.mockRejectedValue({ status: 404, code: "no_backup", message: "no matching backup exists" });
    renderPage();
    await screen.findByText("3 hours ago");
    await userEvent.click(deleteIn("3 hours ago"));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete backup" }));
    expect(await screen.findByText(i18next.t("backups:delete_gone"))).toBeTruthy();
    await vi.waitFor(() => expect(screen.queryByText("3 hours ago")).toBeNull());
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("waits while any restore runs on the server", async () => {
    calls.listBackups.mockResolvedValue({ backups: [backup("bk-1", 3), backup("bk-2", 4)], total: 2 });
    calls.serverJobs.mockResolvedValue([{ name: "restore-survival-aa", kind: "restore", state: "running" }]);
    renderPage();
    await screen.findByText("3 hours ago");
    await vi.waitFor(() => expect(deleteIn("3 hours ago").disabled).toBe(true));
    expect(deleteIn("4 hours ago").disabled).toBe(true);
    expect(deleteIn("3 hours ago").parentElement?.title).toBe(i18next.t("backups:delete_restore_busy"));
  });

  it("waits only on the backup a safety snapshot is about to restore", async () => {
    calls.listBackups.mockResolvedValue({ backups: [backup("bk-1", 3), backup("bk-2", 4)], total: 2 });
    calls.serverJobs.mockResolvedValue([
      { name: "backup-survival-bb", kind: "backup", state: "succeeded", then_restore: "pending", restore_backup_id: "bk-2" },
      { name: "restore-survival-cc", kind: "restore", state: "succeeded" },
    ]);
    renderPage();
    await screen.findByText("4 hours ago");
    await vi.waitFor(() => expect(deleteIn("4 hours ago").disabled).toBe(true));
    expect(deleteIn("3 hours ago").disabled).toBe(false);
    expect(deleteIn("3 hours ago").parentElement?.title).toBe("");
  });

  it("steps back a page when the delete empties the last one", async () => {
    let lastGone = false;
    calls.listBackups.mockImplementation(async ({ offset }: { offset: number }) => ({
      backups: offset === 40 ? (lastGone ? [] : [backup("bk-last", 90)]) : [backup(`bk-${offset}`, 50)],
      total: lastGone ? 40 : 41,
    }));
    calls.deleteBackup.mockImplementation(async () => {
      lastGone = true;
      return { id: "bk-last", status: "expired" };
    });
    renderPage();
    await screen.findByText("Page 1 of 3");
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await screen.findByText("Page 3 of 3");
    await screen.findByText("4 days ago");
    await userEvent.click(deleteIn("4 days ago"));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete backup" }));
    expect(await screen.findByText("Page 2 of 2")).toBeTruthy();
    expect(calls.listBackups).toHaveBeenLastCalledWith({ server: "survival", limit: 20, offset: 20 });
  });

  it("keeps stepping back past pages emptied meanwhile", async () => {
    let gone = false;
    calls.listBackups.mockImplementation(async ({ offset }: { offset: number }) => ({
      backups: offset === 0 ? [backup("bk-0", 50)] : gone ? [] : [backup(`bk-${offset}`, 90)],
      total: gone ? 20 : 41,
    }));
    calls.deleteBackup.mockImplementation(async () => {
      gone = true;
      return { id: "bk-40", status: "expired" };
    });
    renderPage();
    await screen.findByText("Page 1 of 3");
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await screen.findByText("Page 3 of 3");
    await screen.findByText("4 days ago");
    await userEvent.click(deleteIn("4 days ago"));
    await userEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Delete backup" }));
    await vi.waitFor(() => expect(calls.listBackups).toHaveBeenLastCalledWith({ server: "survival", limit: 20, offset: 0 }));
    expect(await screen.findByText("2 days ago")).toBeTruthy();
    expect(screen.queryByText(/^Page \d+ of/)).toBeNull();
  });
});

describe("ServerBackups export", () => {
  const row = (when: string) => screen.getByText(when).closest("tr") as HTMLElement;
  const downloadIn = (when: string) =>
    within(row(when)).getByRole("button", { name: "Download backup" }) as HTMLButtonElement;
  const exportWorld = () => screen.getByRole("button", { name: /^(Export world|Preparing…)$/ }) as HTMLButtonElement;
  const backUp = () => screen.getByRole("button", { name: "Back up now" }) as HTMLButtonElement;
  const text = (key: string, opts?: Record<string, string>) => i18next.t(key, opts);

  // Every hidden link the page clicks, as it was at the click.
  function watchLinks() {
    const clicked: { href: string | null; download: string; hidden: boolean; connected: boolean }[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push({
        href: this.getAttribute("href"),
        download: this.download,
        hidden: this.hidden,
        connected: this.isConnected,
      });
    });
    return clicked;
  }

  // readyAfter answers "pending" for the first n reads of the ticket, then "ready".
  function readyAfter(n: number) {
    let reads = 0;
    calls.exportStatus.mockImplementation(async () => ({ state: reads++ < n ? "pending" : "ready" }));
  }

  beforeEach(() => {
    calls.listBackups.mockResolvedValue({ backups: [backup("bk-1", 3), backup("bk-2", 4)], total: 2 });
    calls.exportDownloadURL.mockImplementation(async (ticket: string) => `/api/v1/exports/${ticket}/download`);
  });

  it("downloads a backup through a hidden link once felis-api holds it, never before", async () => {
    const clicked = watchLinks();
    calls.startBackupExport.mockResolvedValue({ ticket: "t1", state: "pending", filename: "survival-backup-bk-1.tar.gz" });
    readyAfter(2);
    renderPage();
    await screen.findByText("3 hours ago");
    vi.useFakeTimers();
    await act(async () => {
      fireEvent.click(downloadIn("3 hours ago"));
    });
    expect(calls.startBackupExport).toHaveBeenCalledExactlyOnceWith("survival", "bk-1");
    expect(screen.getByRole("status").textContent).toBe(text("backups:export_progress_backup"));
    // One download at a time: every other export waits, and says why.
    expect(downloadIn("3 hours ago").disabled).toBe(true);
    expect(downloadIn("4 hours ago").disabled).toBe(true);
    expect(downloadIn("4 hours ago").parentElement?.title).toBe(text("backups:export_another"));
    expect(exportWorld().disabled).toBe(true);

    await act(() => vi.advanceTimersByTimeAsync(4_000));
    expect(calls.exportStatus).toHaveBeenCalledTimes(2);
    expect(calls.exportStatus).toHaveBeenCalledWith("t1");
    expect(clicked).toEqual([]);

    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(calls.exportDownloadURL).toHaveBeenCalledExactlyOnceWith("t1");
    expect(clicked).toEqual([
      { href: "/api/v1/exports/t1/download", download: "survival-backup-bk-1.tar.gz", hidden: true, connected: true },
    ]);
    expect(document.querySelectorAll("a[download]")).toHaveLength(0);
    expect(screen.getByRole("status").textContent).toBe(
      text("backups:export_started_backup", { filename: "survival-backup-bk-1.tar.gz" }),
    );
    expect(downloadIn("4 hours ago").disabled).toBe(false);
    expect(exportWorld().disabled).toBe(false);

    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(calls.exportStatus).toHaveBeenCalledTimes(3);

    // The next download can start once this one has.
    await act(async () => {
      fireEvent.click(downloadIn("4 hours ago"));
    });
    expect(calls.startBackupExport).toHaveBeenLastCalledWith("survival", "bk-2");
    expect(calls.startBackupExport).toHaveBeenCalledTimes(2);
  });

  it("starts one export for a double click", async () => {
    watchLinks();
    calls.startBackupExport.mockResolvedValue({ ticket: "t1", state: "pending", filename: "a.tar.gz" });
    readyAfter(0);
    renderPage();
    await screen.findByText("3 hours ago");
    await act(async () => {
      fireEvent.click(downloadIn("3 hours ago"));
      fireEvent.click(downloadIn("3 hours ago"));
    });
    expect(calls.startBackupExport).toHaveBeenCalledTimes(1);
  });

  it("exports the stopped world and holds the world buttons meanwhile", async () => {
    const clicked = watchLinks();
    calls.startWorldExport.mockResolvedValue({ ticket: "t2", state: "pending", filename: "survival-world-20231114-221320.tar.gz" });
    readyAfter(1);
    renderPage();
    await screen.findByText("3 hours ago");
    expect(exportWorld().disabled).toBe(false);
    expect(exportWorld().title).toBe(text("backups:export_world_hint"));
    // The export Job holds the world as soon as it exists.
    calls.serverJobs.mockResolvedValue([{ name: "export-survival-aa", kind: "export_world", state: "running" }]);
    vi.useFakeTimers();
    await act(async () => {
      fireEvent.click(exportWorld());
    });
    expect(calls.startWorldExport).toHaveBeenCalledExactlyOnceWith("survival");
    expect(calls.startBackupExport).not.toHaveBeenCalled();
    expect(exportWorld().textContent).toBe("Preparing…");
    expect(screen.getByRole("status").textContent).toBe(text("backups:export_progress_world"));
    expect(backUp().disabled).toBe(true);

    await act(() => vi.advanceTimersByTimeAsync(4_000));
    expect(clicked).toEqual([
      { href: "/api/v1/exports/t2/download", download: "survival-world-20231114-221320.tar.gz", hidden: true, connected: true },
    ]);
    expect(screen.getByRole("status").textContent).toBe(
      text("backups:export_started_world", { filename: "survival-world-20231114-221320.tar.gz" }),
    );
    // Until the download ends the Job still holds the world.
    expect(exportWorld().textContent).toBe("Export world");
    expect(exportWorld().disabled).toBe(true);
    expect(exportWorld().title).toBe(text("backups:export_world_busy"));
    expect(backUp().title).toBe(text("backups:backup_world_busy"));
    expect(screen.getByText("World export", { selector: "li span" })).toBeTruthy();
  });

  it("offers the world export only while the server is stopped", async () => {
    calls.status.mockResolvedValue({ name: "survival", displayName: "Survival", phase: "Running", desiredState: "Running" });
    renderPage();
    await screen.findByText("3 hours ago");
    expect(exportWorld().disabled).toBe(true);
    expect(exportWorld().title).toBe(text("backups:export_requires_stopped"));
    // A backup is still downloadable while the server runs.
    expect(downloadIn("3 hours ago").disabled).toBe(false);
  });

  it("waits for a backup or restore that holds the world, but not for a backup download", async () => {
    calls.serverJobs.mockResolvedValue([{ name: "backup-survival-aa", kind: "backup", state: "running" }]);
    const { unmount } = renderPage();
    await screen.findByText("3 hours ago");
    await vi.waitFor(() => expect(exportWorld().disabled).toBe(true));
    expect(exportWorld().title).toBe(text("backups:export_world_busy"));
    unmount();

    calls.serverJobs.mockResolvedValue([{ name: "export-survival-bb", kind: "export_backup", state: "running" }]);
    renderPage();
    expect(await screen.findByText("Backup download", { selector: "li span" })).toBeTruthy();
    expect(exportWorld().disabled).toBe(false);
    expect(backUp().disabled).toBe(false);
  });

  it("offers a download on exactly the rows a restore is offered on", async () => {
    calls.listBackups.mockResolvedValue({
      backups: [
        backup("bk-ok", 3),
        backup("bk-bad", 4, true),
        { ...backup("bk-old", 5), expires_at: new Date(Date.now() - 3600_000).toISOString() },
      ],
      total: 3,
    });
    renderPage();
    await screen.findByText("3 hours ago");
    for (const [when, offered] of [
      ["3 hours ago", true],
      ["4 hours ago", false],
      ["5 hours ago", false],
    ] as const) {
      expect(within(row(when)).queryByRole("button", { name: "Restore" }) !== null).toBe(offered);
      expect(within(row(when)).queryByRole("button", { name: "Download backup" }) !== null).toBe(offered);
    }
  });

  it("says why a failed export produced nothing", async () => {
    const clicked = watchLinks();
    calls.startBackupExport.mockResolvedValue({ ticket: "t3", state: "pending", filename: "a.tar.gz" });
    calls.exportStatus
      .mockResolvedValueOnce({ state: "pending" })
      .mockResolvedValue({ state: "failed", message: "felis export: ref is not under backup root" });
    renderPage();
    await screen.findByText("3 hours ago");
    vi.useFakeTimers();
    await act(async () => {
      fireEvent.click(downloadIn("3 hours ago"));
    });
    await act(() => vi.advanceTimersByTimeAsync(4_000));
    expect(screen.getByRole("alert").textContent).toBe(
      text("backups:export_failed_because", { reason: "felis export: ref is not under backup root" }),
    );
    expect(clicked).toEqual([]);
    expect(calls.exportDownloadURL).not.toHaveBeenCalled();
    expect(downloadIn("3 hours ago").disabled).toBe(false);
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(calls.exportStatus).toHaveBeenCalledTimes(2);
  });

  it.each([
    ["the limits", "backup", { status: 429, code: "export_busy", message: "one export at a time per user" }, "errors:export_busy"],
    ["a stale ticket", "status", { status: 410, code: "export_expired", message: "this export has expired" }, "errors:export_expired"],
    ["an unwired deployment", "backup", { status: 503, code: "export_unavailable", message: "export is not configured" }, "errors:export_unavailable"],
    ["a server that is not stopped", "world", { status: 409, code: "not_stopped", message: "stop the server" }, "backups:export_requires_stopped"],
  ] as const)("words a refusal for %s", async (_, where, err, key) => {
    const clicked = watchLinks();
    calls.startBackupExport.mockResolvedValue({ ticket: "t4", state: "pending", filename: "a.tar.gz" });
    calls.startWorldExport.mockRejectedValue(err);
    if (where === "backup") calls.startBackupExport.mockRejectedValue(err);
    calls.exportStatus.mockRejectedValue(err);
    renderPage();
    await screen.findByText("3 hours ago");
    vi.useFakeTimers();
    await act(async () => {
      fireEvent.click(where === "world" ? exportWorld() : downloadIn("3 hours ago"));
    });
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(screen.getByRole("alert").textContent).toBe(text(key));
    expect(clicked).toEqual([]);
    expect(exportWorld().disabled).toBe(false);
  });

  it("stops reading the ticket once the page is left", async () => {
    const clicked = watchLinks();
    calls.startBackupExport.mockResolvedValue({ ticket: "t5", state: "pending", filename: "a.tar.gz" });
    readyAfter(1);
    const { unmount } = renderPage();
    await screen.findByText("3 hours ago");
    vi.useFakeTimers();
    await act(async () => {
      fireEvent.click(downloadIn("3 hours ago"));
    });
    await act(() => vi.advanceTimersByTimeAsync(2_000));
    expect(calls.exportStatus).toHaveBeenCalledTimes(1);
    unmount();
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(calls.exportStatus).toHaveBeenCalledTimes(1);
    expect(clicked).toEqual([]);
  });
});
