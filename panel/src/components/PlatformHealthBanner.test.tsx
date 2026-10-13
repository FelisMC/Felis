// @vitest-environment jsdom
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PlatformHealthBanner } from "./PlatformHealthBanner";
import type { HostOverview } from "@/lib/types";

const calls = vi.hoisted(() => ({ hostOverview: vi.fn() }));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: calls }));

const overview: HostOverview = {
  host: "ctl-a", at: "2026-10-12T00:00:00Z", version: "v1", disks: [{ path: "/var", total: 100, available: 5 }], units: [], proxy: {},
  alerts: [], databaseBackups: [], offsite: { configured: false, keyKept: false },
  settings: { dbBackupTime: "03:30", dbBackupKeep: 14, velocityXmx: "1G" },
  connection: { mode: "local", panelHost: "console.example.com", adminHost: "op.console.example.com" },
  storage: { mode: "local" }, rootDomain: "example.com",
};

beforeEach(() => vi.clearAllMocks());

describe("dashboard health banner", () => {
  it("names what needs attention and links to it", async () => {
    calls.hostOverview.mockResolvedValue({ ...overview, alerts: [{ key: "disk/var", severity: "critical", summary: "", summary_en: "x", firstSeen: "2026-10-12T00:00:00Z" }] });
    render(<MemoryRouter><PlatformHealthBanner /></MemoryRouter>);
    const alerts = await screen.findByRole("link", { name: "1 open alert" });
    expect(alerts.getAttribute("href")).toBe("/admin/updates?tab=status");
    expect(screen.getByRole("link", { name: "No database backups on this host" }).getAttribute("href")).toBe("/admin/updates?tab=backups");
    expect(screen.getByRole("link", { name: "Off-site copy: Not configured" }).getAttribute("href")).toBe("/admin/updates?tab=offsite");
    expect(screen.getByRole("link", { name: /free on \/var/ }).className).toContain("text-destructive");
  });

  it("stays out of the way when the host cannot be read", async () => {
    calls.hostOverview.mockRejectedValue({ status: 503, code: "node_control_unavailable", message: "x" });
    const { container } = render(<MemoryRouter><PlatformHealthBanner /></MemoryRouter>);
    await waitFor(() => expect(calls.hostOverview).toHaveBeenCalled());
    expect(container.textContent).toBe("");
  });
});
