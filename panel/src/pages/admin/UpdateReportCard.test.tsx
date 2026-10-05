// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import i18next from "i18next";
import { UpdateReportCard } from "./UpdateReportCard";
import type { UpdateReport } from "@/lib/types";

const calls = vi.hoisted(() => ({ getUpdateReport: vi.fn() }));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const CHECKED = new Date(Date.now() - 7 * 3600 * 1000).toISOString();

const WITH_UPDATES: UpdateReport = {
  report: {
    checked_at: CHECKED,
    felis: "v0.4.0",
    components: [
      { name: "felis-api", current: "v0.4.0", latest: "v0.5.1", state: "available", selector: "panel" },
      { name: "k3s", current: "v1.36.2+k3s1", state: "current", selector: "k3s" },
      { name: "cloudflared", current: "2026.6.1", latest: "2026.9.0", state: "available", selector: "cloudflared" },
      { name: "jre", current: "21.0.8+9", state: "unknown", selector: "jre", error: "adoptium: context deadline exceeded" },
      { name: "postgresql", state: "unreadable", selector: "postgres", error: "psql: not found", note: "PostgreSQL 13 is past its end of life" },
    ],
  },
  stale: false,
  max_age_seconds: 93600,
};

beforeEach(() => {
  calls.getUpdateReport.mockReset();
});
afterEach(() => {
  vi.restoreAllMocks();
  return i18next.changeLanguage("en-US");
});

function rowText(name: string): string {
  const row = screen.getByText(name).closest("li");
  if (!row) throw new Error(`no row for ${name}`);
  return row.textContent ?? "";
}

describe("UpdateReportCard", () => {
  it("lists each component's state and hands over the host command for the ones with updates", async () => {
    calls.getUpdateReport.mockResolvedValue(WITH_UPDATES);
    render(<UpdateReportCard />);
    expect(await screen.findByText("2 updates available")).toBeTruthy();
    expect(screen.getByText("Checked 7 hours ago by felis v0.4.0")).toBeTruthy();
    expect(rowText("felis-api")).toBe("felis-apiInstalled: v0.4.0Newer release: v0.5.1Update available");
    expect(rowText("k3s")).toBe("k3sInstalled: v1.36.2+k3s1Newer release: —Up to date");
    expect(rowText("jre")).toBe("jreInstalled: 21.0.8+9Newer release: —Feed unreachableadoptium: context deadline exceeded");
    expect(rowText("postgresql")).toBe(
      "postgresqlInstalled: —Newer release: —Version read failedpsql: not foundPostgreSQL 13 is past its end of life",
    );
    expect(screen.getByTitle("sudo felis update --panel --cloudflared").textContent).toBe("sudo felis update --panel --cloudflared");
    expect(screen.queryByText("sudo felis update --record")).toBeNull();
  });

  it("says up to date and offers no apply command when nothing is newer", async () => {
    calls.getUpdateReport.mockResolvedValue({
      ...WITH_UPDATES,
      report: { ...WITH_UPDATES.report!, components: [{ name: "k3s", current: "v1.36.2+k3s1", state: "current", selector: "k3s" }] },
    });
    render(<UpdateReportCard />);
    expect(await screen.findByText("k3s")).toBeTruthy();
    const title = screen.getByText("Component versions").closest("div")!;
    expect(within(title).getByText("Up to date")).toBeTruthy();
    expect(document.body.textContent).not.toContain("sudo felis update");
  });

  it("tells the admin how to run the check when the timer never recorded one", async () => {
    calls.getUpdateReport.mockResolvedValue({ report: null, stale: true, max_age_seconds: 93600 });
    render(<UpdateReportCard />);
    expect(await screen.findByText("Never checked")).toBeTruthy();
    expect(screen.getByText("No version check records")).toBeTruthy();
    expect(screen.getByTitle("sudo felis update --record")).toBeTruthy();
    expect(screen.getByTitle("journalctl -u felis-update-check -n 50 --no-pager")).toBeTruthy();
  });

  it("flags an overdue check and keeps its versions visible", async () => {
    calls.getUpdateReport.mockResolvedValue({ ...WITH_UPDATES, stale: true });
    render(<UpdateReportCard />);
    expect(await screen.findByText("Check overdue")).toBeTruthy();
    expect(screen.getByText("The newest version check is more than 26 hours old")).toBeTruthy();
    expect(rowText("felis-api")).toContain("v0.5.1");
    // A stale list is not a basis to apply from: the apply hint waits for a fresh check.
    expect(screen.queryByTitle("sudo felis update --panel --cloudflared")).toBeNull();
    expect(screen.getByTitle("sudo felis update --record")).toBeTruthy();
  });
});
