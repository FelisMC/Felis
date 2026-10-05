// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import i18next from "i18next";
import { DBBackupCard } from "./DBBackupCard";
import type { DBBackupStatus } from "@/lib/types";

const calls = vi.hoisted(() => ({ getDBBackup: vi.fn() }));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const AT = new Date(Date.now() - 3 * 3600 * 1000).toISOString();
const REASON = "k3s kubectl get minecraftservers: exit status 1: connection refused (tried 3 times)";

function status(last: Partial<NonNullable<DBBackupStatus["last"]>>, stale = false): DBBackupStatus {
  return {
    last: {
      at: AT,
      name: "felis-db-20260926T033000Z-daily.tar",
      label: "daily",
      size_bytes: 4 << 20,
      schema_version: 42,
      dir: "/var/lib/felis/db-backups",
      ...last,
    },
    stale,
    max_age_seconds: 93600,
  };
}

beforeEach(() => {
  calls.getDBBackup.mockReset();
});
afterEach(() => {
  vi.restoreAllMocks();
  return i18next.changeLanguage("en-US");
});

describe("DBBackupCard", () => {
  it("shows a whole, fresh backup as healthy", async () => {
    calls.getDBBackup.mockResolvedValue(status({}));
    render(<DBBackupCard />);
    expect(await screen.findByText("Healthy")).toBeTruthy();
    expect(screen.queryByText("Incomplete")).toBeNull();
    expect(screen.queryByText("The newest backup lacks the server definitions")).toBeNull();
  });

  it("warns that a fresh backup without the MinecraftServer objects restores no servers", async () => {
    calls.getDBBackup.mockResolvedValue(status({ servers_error: REASON }));
    render(<DBBackupCard />);
    expect(await screen.findByText("Incomplete")).toBeTruthy();
    expect(screen.queryByText("Healthy")).toBeNull();
    expect(screen.getByText("The newest backup lacks the server definitions")).toBeTruthy();
    expect(screen.getByText(/Restoring the database alone cannot recreate servers/)).toBeTruthy();
    expect(screen.getByText(REASON)).toBeTruthy();
    expect(screen.getByTitle("sudo k3s kubectl get minecraftservers -A")).toBeTruthy();
    expect(screen.getByTitle("sudo felis db backup")).toBeTruthy();
  });

  it("keeps the overdue alarm first and still names the missing servers", async () => {
    calls.getDBBackup.mockResolvedValue(status({ servers_error: REASON }, true));
    render(<DBBackupCard />);
    expect(await screen.findByText("Overdue")).toBeTruthy();
    expect(screen.queryByText("Incomplete")).toBeNull();
    expect(screen.getByText("The newest backup lacks the server definitions")).toBeTruthy();
  });

  it("turns an old daily backup red and hands over the daily unit", async () => {
    const old = new Date(Date.now() - 30 * 3600 * 1000).toISOString();
    calls.getDBBackup.mockResolvedValue(status({ at: old, daily_at: old }, true));
    render(<DBBackupCard />);
    expect(await screen.findByText("Overdue")).toBeTruthy();
    expect(screen.getByText("The newest backup is more than 26 hours old")).toBeTruthy();
    expect(screen.getByText(/inspect the timer logs:$/)).toBeTruthy();
    expect(screen.getByText("yesterday").className).toBe("text-destructive");
    expect(screen.getByTitle("sudo systemctl start felis-db-backup.service")).toBeTruthy();
  });

  it("names the stopped daily timer under a fresh manual backup", async () => {
    const daily = new Date(Date.now() - 50 * 3600 * 1000).toISOString();
    calls.getDBBackup.mockResolvedValue(
      status({ label: "manual", name: "felis-db-20260926T101500Z-manual.tar", daily_at: daily }, true),
    );
    render(<DBBackupCard />);
    expect(await screen.findByText("Overdue")).toBeTruthy();
    expect(screen.getByText("The daily backup last completed 2 days ago, past the 26-hour limit")).toBeTruthy();
    expect(screen.getByText(/^The newer backup was created manually or by another task and is available for restoration/)).toBeTruthy();
    // The manual bundle itself is fresh: its age stays out of the alarm.
    expect(screen.getByText("3 hours ago").className).toBe("");
    expect(screen.getByTitle("sudo systemctl start felis-db-backup.service")).toBeTruthy();
  });

  it("says the daily timer never completed when only other kinds exist", async () => {
    calls.getDBBackup.mockResolvedValue(status({ label: "pre-migrate", name: "felis-db-20260926T101500Z-pre-migrate.tar" }, true));
    render(<DBBackupCard />);
    expect(await screen.findByText("The daily timer has not completed a backup")).toBeTruthy();
    expect(screen.getByText(/^The newer backup was created manually/)).toBeTruthy();
  });

  it("names an off-site copy snapshot by its kind", async () => {
    calls.getDBBackup.mockResolvedValue(status({ label: "offsite", name: "felis-db-20260926T101500Z-offsite.tar" }));
    render(<DBBackupCard />);
    expect(await screen.findByText("Off-site copy snapshot")).toBeTruthy();
  });
});
