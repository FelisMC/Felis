// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ServerConsole } from "./ServerConsole";

const calls = vi.hoisted(() => ({ status: vi.fn(), myServers: vi.fn() }));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({
    loading: false,
    identity: { user_id: "admin-1", email: "admin@example.test", role: "admin" },
    isAdmin: true,
    isOwner: false,
  }),
}));
vi.mock("@/lib/config", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/config")>();
  return {
    ...actual,
    loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "example.test", gamePort: 25570 }),
  };
});
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});
// The live log is a WebSocket; here it only matters whether the page shows it.
vi.mock("@/components/LogConsole", () => ({ LogConsole: () => <div data-testid="log-stream" /> }));

beforeEach(() => {
  calls.status.mockReset();
  calls.myServers.mockReset();
  calls.myServers.mockResolvedValue([]);
});

function renderConsole() {
  render(
    <MemoryRouter initialEntries={["/servers/survival"]}>
      <Routes>
        <Route path="/servers/:name" element={<ServerConsole />} />
      </Routes>
    </MemoryRouter>,
  );
}

function status(over: Record<string, unknown>) {
  return { name: "survival", subdomain: "survival", phase: "Failed", ready: false, playersOnline: 0, playersMax: 20, ...over };
}

describe("ServerConsole failed start", () => {
  it("shows the failed attempt's log under what the owner can do once retries are spent", async () => {
    calls.status.mockResolvedValue(status({ desiredState: "Running", startGaveUp: true, autoRestarts: 3 }));
    renderConsole();

    const notice = await screen.findByRole("status", { name: "Server failed to start" });
    expect(within(notice).getByText(/automatic retries are spent/)).toBeTruthy();
    expect(await screen.findByTestId("log-stream")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Retry start/ })).toBeTruthy();
  });

  it("counts the retries used while the operator is still retrying", async () => {
    calls.status.mockResolvedValue(status({ desiredState: "Running", autoRestarts: 1 }));
    renderConsole();

    const notice = await screen.findByRole("status", { name: "Start timed out — retrying" });
    expect(within(notice).getByText(/1 of 3 retries used/)).toBeTruthy();
    expect(screen.getByText("Timed out · retrying")).toBeTruthy();
  });

  it("asks nothing of the owner once a failed server is being stopped", async () => {
    calls.status.mockResolvedValue(status({ desiredState: "Stopped" }));
    renderConsole();

    expect(await screen.findByTestId("log-stream")).toBeTruthy();
    expect(screen.queryByRole("status", { name: /failed to start|timed out/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Retry start/ })).toBeNull();
  });
});
