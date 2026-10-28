// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import type { AuditEntry, MyServerView, PlatformUsage, WhitelistImage } from "@/lib/types";
import { Dashboard } from "./Dashboard";

const tier = vi.hoisted(() => ({ isAdmin: false, isOwner: false }));
const calls = vi.hoisted(() => ({
  myServers: vi.fn(),
  linkStatus: vi.fn(),
  listImages: vi.fn(),
  listAudit: vi.fn(),
  hostOverview: vi.fn(),
  fleetUsage: vi.fn(),
  me: vi.fn(),
}));

vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
vi.mock("@/lib/webgl", () => ({ webglAvailable: () => false }));
const deployment = vi.hoisted(() => ({ distributed: false }));
vi.mock("@/lib/config", () => ({
  loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "example.test", distributed: deployment.distributed }),
}));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string, options?: Record<string, unknown>) => i18next.t(key, { ns: "dashboard", ...options });
const failure = (message: string) => ({ status: 409, code: "test_failure", message });
const never = () => new Promise<never>(() => {});

const lobby: MyServerView = {
  name: "lobby",
  subdomain: "lobby",
  owned: true,
  claimable: false,
  phase: "Running",
  playersOnline: 3,
  playersMax: 20,
};

function renderDashboard() {
  return render(
    <MemoryRouter>
      <Dashboard />
    </MemoryRouter>,
  );
}

// The number shown under one of the admin spec labels.
function specValue(label: string) {
  const cell = screen.getByText(t(label)).closest("div.space-y-1");
  return cell?.lastElementChild?.textContent;
}

beforeEach(() => {
  tier.isAdmin = false;
  tier.isOwner = false;
  for (const fn of Object.values(calls)) fn.mockReset();
  calls.myServers.mockResolvedValue([lobby]);
  calls.linkStatus.mockResolvedValue({ linked: true });
  calls.listImages.mockResolvedValue([]);
  calls.listAudit.mockResolvedValue({ entries: [], next_cursor: "" });
  calls.hostOverview.mockImplementation(never);
  calls.fleetUsage.mockImplementation(never);
  calls.me.mockImplementation(never);
});

describe("Dashboard", () => {
  it("keeps the fleet on screen when link status fails, and retries only the link card", async () => {
    calls.linkStatus.mockRejectedValueOnce(failure("link backend down"));
    renderDashboard();

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(t("link_status_failed"))).toBeTruthy();
    expect(within(alert).getByText("link backend down")).toBeTruthy();
    expect(screen.getByText(t("stat_servers"))).toBeTruthy();
    expect(screen.queryByText(t("account_unlinked_title"))).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: i18next.t("common:try_again") }));

    expect(await screen.findByText(t("account_linked_title"))).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(calls.linkStatus).toHaveBeenCalledTimes(2);
    expect(calls.myServers).toHaveBeenCalledTimes(1);
  });

  it("retries the server list from the page error", async () => {
    calls.myServers.mockRejectedValueOnce(failure("servers unavailable"));
    renderDashboard();

    expect(await screen.findByText("servers unavailable")).toBeTruthy();
    expect(screen.queryByText(t("stat_servers"))).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: i18next.t("common:try_again") }));

    expect(await screen.findByText(t("stat_servers"))).toBeTruthy();
    expect(calls.myServers).toHaveBeenCalledTimes(2);
  });

  it("says it is checking while link status loads, without the unlinked warning", async () => {
    calls.linkStatus.mockImplementation(never);
    renderDashboard();

    expect(await screen.findByText(t("link_status_checking"))).toBeTruthy();
    expect(screen.queryByText(t("account_unlinked_title"))).toBeNull();
    expect(screen.queryByText(t("go_link"))).toBeNull();
  });

  it("warns an unlinked user once link status says so", async () => {
    calls.linkStatus.mockResolvedValue({ linked: false });
    renderDashboard();

    expect(await screen.findByText(t("account_unlinked_title"))).toBeTruthy();
    expect(screen.queryByText(t("link_status_checking"))).toBeNull();
  });

  it("takes admin from the tier and counts the image whitelist", async () => {
    tier.isAdmin = true;
    const images: WhitelistImage[] = [
      { image_ref: "a", enabled: true, source: "built", added_by: "o", added_at: "" },
      { image_ref: "b", enabled: true, source: "external", added_by: "o", added_at: "" },
      { image_ref: "c", enabled: true, source: "external", added_by: "o", added_at: "" },
    ];
    calls.listImages.mockResolvedValue(images);
    renderDashboard();

    await waitFor(() => expect(specValue("spec_images_total")).toBe("3"));
    expect(specValue("spec_images_built")).toBe("1");
    expect(specValue("spec_images_external")).toBe("2");
    expect(calls.me).not.toHaveBeenCalled();
  });

  it("shows a dash for image counts it could not read, instead of zero", async () => {
    tier.isAdmin = true;
    calls.listImages.mockRejectedValue(failure("forbidden"));
    renderDashboard();

    await screen.findByText(t("spec_images_total"));
    await waitFor(() => expect(calls.listImages).toHaveBeenCalled());
    // Let the rejection land: a loading whitelist shows a dash too.
    await act(() => new Promise((resolve) => setTimeout(resolve, 10)));
    expect(specValue("spec_images_total")).toBe("—");
    expect(specValue("spec_images_built")).toBe("—");
    expect(specValue("spec_images_external")).toBe("—");
    expect(screen.getByText(t("stat_servers"))).toBeTruthy();
  });

  it("shows a dash while the whitelist is still loading", async () => {
    tier.isAdmin = true;
    calls.listImages.mockImplementation(never);
    renderDashboard();

    await screen.findByText(t("spec_images_total"));
    expect(specValue("spec_images_total")).toBe("—");
    expect(specValue("spec_images_built")).toBe("—");
  });

  it("does not list images for a user", async () => {
    renderDashboard();

    await screen.findByText(t("welcome_title"));
    expect(calls.listImages).not.toHaveBeenCalled();
  });
});

describe("Dashboard recent actions", () => {
  const recent: AuditEntry = {
    id: 7,
    created_at: "2026-10-01T00:00:00Z",
    actor: "owner@example.net",
    actor_user_id: null,
    source: "external",
    action: "server.patch",
    server_name: "lobby",
    request_id: null,
    client_ip: null,
    user_agent: null,
    payload: null,
  };

  it("lists the latest ten for the Owner and links to the whole log", async () => {
    tier.isAdmin = true;
    tier.isOwner = true;
    calls.listAudit.mockResolvedValue({ entries: [recent], next_cursor: "" });
    renderDashboard();

    expect(await screen.findByText(recent.action)).toBeTruthy();
    expect(screen.getByText(t("audit_title"))).toBeTruthy();
    expect(screen.getByText(recent.actor)).toBeTruthy();
    expect(calls.listAudit).toHaveBeenCalledWith({ limit: 10 });
    expect(screen.getByRole("link", { name: t("audit_view_all") }).getAttribute("href")).toBe("/admin/audit");
  });

  it("stays out of an admin's dashboard without asking for the log", async () => {
    tier.isAdmin = true;
    renderDashboard();

    await screen.findByText(t("stat_servers"));
    expect(screen.queryByText(t("audit_title"))).toBeNull();
    expect(calls.listAudit).not.toHaveBeenCalled();
  });
});

describe("Dashboard platform data", () => {
  beforeEach(() => {
    deployment.distributed = true;
  });
  afterEach(() => {
    deployment.distributed = false;
  });
  const at = (secondsAgo: number) => new Date(Date.now() - secondsAgo * 1000).toISOString();
  const usage: PlatformUsage = {
    current: { at: at(5), cpuMilli: 1250, memoryBytes: 3 * 1024 ** 3, running: 7 },
    history: [
      { at: at(35), cpuMilli: 1000, memoryBytes: 2 * 1024 ** 3, running: 6 },
      { at: at(20), cpuMilli: 1100, memoryBytes: 2 * 1024 ** 3, running: 7 },
      { at: at(5), cpuMilli: 1250, memoryBytes: 3 * 1024 ** 3, running: 7 },
    ],
    nodes: [
      { name: "worker-a", cpuMilli: 750, memoryBytes: 1024 ** 3, running: 3 },
      { name: "worker-b", cpuMilli: 500, memoryBytes: 2 * 1024 ** 3, running: 4 },
    ],
    loginFailures24h: 6,
  };

  it("shows an admin the totals, the three curves and each node", async () => {
    tier.isAdmin = true;
    calls.fleetUsage.mockResolvedValue(usage);
    renderDashboard();

    expect(await screen.findByText(t("platform_title"))).toBeTruthy();
    for (const curve of ["platform_cpu_curve", "platform_memory_curve", "platform_running_curve"]) {
      expect(screen.getByRole("img", { name: t(curve) })).toBeTruthy();
    }
    expect(screen.getByText(t("platform_cpu_value", { cores: "1.25" }))).toBeTruthy();
    expect(screen.getByText("3.0 GiB")).toBeTruthy();
    expect(screen.getByText("7")).toBeTruthy();
    const nodes = within(screen.getByRole("table"));
    expect(nodes.getByText("worker-a")).toBeTruthy();
    expect(nodes.getByText(t("platform_cpu_value", { cores: "0.75" }))).toBeTruthy();
    expect(nodes.getByText("2.0 GiB")).toBeTruthy();
    expect(nodes.getByText("4")).toBeTruthy();
    expect(screen.getByText("6")).toBeTruthy();
  });

  it("says so when nothing is known yet or nothing runs, and still draws the curves", async () => {
    tier.isAdmin = true;
    calls.fleetUsage.mockResolvedValue({ ...usage, current: null, nodes: [] });
    renderDashboard();

    expect(await screen.findByText(t("platform_nodes_empty"))).toBeTruthy();
    expect(screen.getAllByText(t("platform_no_data"))).toHaveLength(3);
    expect(screen.getByRole("img", { name: t("platform_cpu_curve") })).toBeTruthy();
    expect(screen.getByRole("img", { name: t("platform_running_curve") })).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("leaves the node table out of a single-node install", async () => {
    deployment.distributed = false;
    tier.isAdmin = true;
    calls.fleetUsage.mockResolvedValue(usage);
    renderDashboard();

    expect(await screen.findByText(t("platform_cpu_value", { cores: "1.25" }))).toBeTruthy();
    expect(screen.queryByText(t("platform_nodes_title"))).toBeNull();
    expect(screen.queryByText("worker-a")).toBeNull();
  });

  it("neither shows nor asks for platform data on a user's dashboard", async () => {
    renderDashboard();

    await screen.findByText(t("welcome_title"));
    expect(screen.queryByText(t("platform_title"))).toBeNull();
    expect(calls.fleetUsage).not.toHaveBeenCalled();
  });

  it("links the Owner to the audit log and not an admin", async () => {
    tier.isAdmin = true;
    calls.fleetUsage.mockResolvedValue(usage);
    const view = renderDashboard();
    await screen.findByText(t("platform_title"));
    await screen.findByText("worker-a");
    expect(screen.queryByRole("link", { name: t("platform_view_audit") })).toBeNull();

    view.unmount();
    tier.isOwner = true;
    renderDashboard();
    const link = await screen.findByRole("link", { name: t("platform_view_audit") });
    expect(link.getAttribute("href")).toBe("/admin/audit");
  });
});

describe("Dashboard following the fleet", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  // The fleet grid names each server with its phase.
  const tile = () => screen.getByTitle(/^lobby · /).getAttribute("title");
  const phase = (p: string) => `lobby · ${i18next.t(`servers:phase_${p}`)}`;

  it("rereads the servers on its own, and keeps the last read when a reread fails", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.myServers.mockResolvedValue([{ ...lobby, phase: "Stopped", desiredState: "Stopped", playersOnline: 0 }]);
    renderDashboard();
    await screen.findByTitle(/^lobby · /);
    expect(tile()).toBe(phase("stopped"));

    calls.myServers.mockResolvedValue([lobby]);
    await act(() => vi.advanceTimersByTimeAsync(9_000));
    expect(calls.myServers).toHaveBeenCalledTimes(1);
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    expect(calls.myServers).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(tile()).toBe(phase("running")));

    calls.myServers.mockRejectedValue(failure("servers unavailable"));
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("servers unavailable");
    expect(tile()).toBe(phase("running"));
  });

  it("counts a server just woken as starting", async () => {
    calls.myServers.mockResolvedValue([{ ...lobby, phase: "Stopped", desiredState: "Running", playersOnline: 0 }]);
    renderDashboard();
    await screen.findByTitle(/^lobby · /);
    expect(tile()).toBe(phase("starting"));
  });
});
