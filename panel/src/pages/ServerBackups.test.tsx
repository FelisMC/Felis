// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { ServerBackups } from "./ServerBackups";
import type { BackupView } from "@/lib/types";

const calls = vi.hoisted(() => ({
  status: vi.fn(),
  myServers: vi.fn(),
  serverJobs: vi.fn(),
  listBackups: vi.fn(),
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
});
