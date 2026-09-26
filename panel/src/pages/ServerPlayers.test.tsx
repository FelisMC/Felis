// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ServerPlayers } from "./ServerPlayers";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS } from "@/lib/hooks";

const calls = vi.hoisted(() => ({ status: vi.fn(), myServers: vi.fn() }));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({
    loading: false,
    identity: { user_id: "admin-1", email: "admin@example.test", role: "admin" },
    isAdmin: true,
    isOwner: false,
  }),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});
// Each section reads over RCON on its own; here it only matters whether the page shows them.
vi.mock("@/components/players/OnlineSection", () => ({ OnlineSection: () => <div data-testid="online" /> }));
vi.mock("@/components/players/WhitelistSection", () => ({ WhitelistSection: () => <div /> }));
vi.mock("@/components/players/BansSection", () => ({ BansSection: () => <div /> }));
// The wake list reads Postgres, so the page shows it whether the server runs or not.
vi.mock("@/components/players/WakeListSection", () => ({
  WakeListSection: (p: { policy?: string; defaultOpen?: boolean }) => (
    <div data-testid="wake-list" data-policy={p.policy} data-open={String(!!p.defaultOpen)} />
  ),
}));

const status = (over: Record<string, unknown>) => ({ name: "lobby", subdomain: "lobby", ready: false, ...over });

beforeEach(() => {
  calls.status.mockReset();
  calls.myServers.mockReset();
  calls.myServers.mockResolvedValue([]);
});
afterEach(() => {
  vi.useRealTimers();
});

function renderPage() {
  render(
    <MemoryRouter initialEntries={["/servers/lobby/players"]}>
      <Routes>
        <Route path="/servers/:name/players" element={<ServerPlayers />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("ServerPlayers following the server", () => {
  it("switches over once the server is up, and back when it stops", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped", autostartPolicy: "allowlist" }));
    renderPage();
    expect(await screen.findByText("Server is asleep")).toBeTruthy();
    // Asleep, the wake list is the one block left, so it opens.
    expect(screen.getByTestId("wake-list").dataset).toMatchObject({ policy: "allowlist", open: "true" });

    // Waiting on the server: the fast pace.
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running", ready: true }));
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS));
    expect(await screen.findByTestId("online")).toBeTruthy();
    expect(screen.getByTestId("wake-list").dataset.open).toBe("false");

    // Running: the slow pace, which still sees an idle stop.
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped" }));
    const reads = calls.status.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS));
    expect(calls.status.mock.calls.length).toBe(reads);
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_SLOW_MS - STATUS_POLL_FAST_MS));
    expect(await screen.findByText("Server is asleep")).toBeTruthy();
    expect(screen.queryByTestId("online")).toBeNull();
  });

  it("keeps the page when a reread fails, and says the status may be old", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running", ready: true }));
    renderPage();
    expect(await screen.findByTestId("online")).toBeTruthy();

    calls.status.mockRejectedValue({ status: 409, code: "test_failure", message: "status backend down" });
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_SLOW_MS));
    expect((await screen.findByRole("alert")).textContent).toContain("status backend down");
    expect(screen.getByTestId("online")).toBeTruthy();
  });
});

describe("ServerPlayers on a server asked to move", () => {
  it("says a woken server is starting and one asked to stop is shutting down", async () => {
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Running" }));
    const first = render(
      <MemoryRouter initialEntries={["/servers/lobby/players"]}>
        <Routes>
          <Route path="/servers/:name/players" element={<ServerPlayers />} />
        </Routes>
      </MemoryRouter>,
    );
    expect(await screen.findByText("Server is starting")).toBeTruthy();
    first.unmount();

    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Stopped", ready: true }));
    renderPage();
    expect(await screen.findByText("Server is shutting down")).toBeTruthy();
    expect(screen.queryByTestId("online")).toBeNull();
  });
});
