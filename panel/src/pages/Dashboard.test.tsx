// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import type { MyServerView, WhitelistImage } from "@/lib/types";
import { Dashboard } from "./Dashboard";

const tier = vi.hoisted(() => ({ isAdmin: false }));
const calls = vi.hoisted(() => ({
  myServers: vi.fn(),
  linkStatus: vi.fn(),
  listImages: vi.fn(),
  me: vi.fn(),
}));

vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
vi.mock("@/lib/webgl", () => ({ webglAvailable: () => false }));
vi.mock("@/lib/config", () => ({
  loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "example.test" }),
}));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string) => i18next.t(key, { ns: "dashboard" });
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
  for (const fn of Object.values(calls)) fn.mockReset();
  calls.myServers.mockResolvedValue([lobby]);
  calls.linkStatus.mockResolvedValue({ linked: true });
  calls.listImages.mockResolvedValue([]);
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
