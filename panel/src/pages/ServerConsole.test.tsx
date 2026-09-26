// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ServerConsole } from "./ServerConsole";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS } from "@/lib/hooks";

const calls = vi.hoisted(() => ({ status: vi.fn(), myServers: vi.fn(), listImages: vi.fn() }));
const tier = vi.hoisted(() => ({
  loading: false,
  identity: { user_id: "admin-1", email: "admin@example.test", role: "admin" },
  isAdmin: true,
  isOwner: false,
}));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
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
  tier.isAdmin = true;
  calls.status.mockReset();
  calls.myServers.mockReset();
  calls.myServers.mockResolvedValue([]);
  calls.listImages.mockReset();
  calls.listImages.mockResolvedValue([]);
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

describe("ServerConsole edit dialog", () => {
  it("offers the server's memory limit as the current memory, not the JVM heap", async () => {
    calls.status.mockResolvedValue(
      status({ phase: "Stopped", desiredState: "Stopped", memory: "4Gi", javaMemory: "3072M", cpu: "2" }),
    );
    renderConsole();

    await userEvent.setup().click(await screen.findByRole("button", { name: /Edit Server Config/ }));
    const memory = await screen.findByRole("combobox", { name: "Memory" });
    expect(within(memory).getByText("4Gi")).toBeTruthy();
    expect(screen.queryByText("3072M")).toBeNull();
    expect((screen.getByLabelText("CPU Limit") as HTMLInputElement).value).toBe("2");
  });
});

describe("ServerConsole following the server", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  const advance = (ms: number) => act(() => vi.advanceTimersByTimeAsync(ms));

  it("picks up a server woken elsewhere without a reload", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped" }));
    renderConsole();
    expect(await screen.findByText("Server is asleep")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Wake" })).toBeTruthy();

    // Nothing is due, so it rereads at the slow pace.
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running", ready: true }));
    await advance(STATUS_POLL_SLOW_MS - 1_000);
    expect(calls.status).toHaveBeenCalledTimes(1);
    await advance(1_000);
    expect(await screen.findByTestId("log-stream")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
    expect(screen.queryByText("Server is asleep")).toBeNull();
  });

  it("shows a wake at once and rereads fast until the pod is up", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Running" }));
    renderConsole();
    expect(await screen.findByText("Server is starting")).toBeTruthy();
    expect(screen.getByText("Starting")).toBeTruthy();
    expect(screen.queryByText("Stopped")).toBeNull();
    // Already asked to run: a second Wake would only be refused; Stop is the way out.
    expect(screen.queryByRole("button", { name: "Wake" })).toBeNull();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();

    calls.status.mockResolvedValue(status({ phase: "Starting", desiredState: "Running" }));
    await advance(STATUS_POLL_FAST_MS);
    expect(calls.status).toHaveBeenCalledTimes(2);
    expect(await screen.findByTestId("log-stream")).toBeTruthy();
  });

  it("keeps the console when a reread fails, and says the status may be old", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running", ready: true }));
    renderConsole();
    expect(await screen.findByTestId("log-stream")).toBeTruthy();

    calls.status.mockRejectedValue({ status: 409, code: "test_failure", message: "status backend down" });
    await advance(STATUS_POLL_SLOW_MS);
    await waitFor(() => expect(screen.getByText(/Couldn't refresh/)).toBeTruthy());
    expect(screen.getByText(/status backend down/)).toBeTruthy();
    expect(screen.getByTestId("log-stream")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
  });
});

describe("ServerConsole retirement", () => {
  const stopped = (over: Record<string, unknown> = {}) => status({ phase: "Stopped", desiredState: "Stopped", ...over });
  const mine = (owned: boolean) => [
    { name: "survival", subdomain: "survival", owned, claimable: false, playersOnline: 0, playersMax: 20 },
  ];
  const giveUp = () => screen.queryByRole("button", { name: "Give up" });
  const del = () => screen.queryByRole("button", { name: "Delete" });

  it("offers the owner a give-up and no deletion", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue(mine(true));
    calls.status.mockResolvedValue(stopped());
    renderConsole();

    expect(await screen.findByRole("button", { name: "Give up" })).toBeTruthy();
    expect(del()).toBeNull();
    expect(screen.getByRole("button", { name: "Wake" })).toBeTruthy();
  });

  it("offers an admin both", async () => {
    calls.status.mockResolvedValue(stopped());
    renderConsole();

    expect(await screen.findByRole("button", { name: "Delete" })).toBeTruthy();
    expect(giveUp()).toBeTruthy();
  });

  it("offers someone who does not own it neither", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue(mine(false));
    calls.status.mockResolvedValue(stopped());
    renderConsole();

    expect(await screen.findByText("Server is asleep")).toBeTruthy();
    await waitFor(() => expect(calls.myServers).toHaveBeenCalled());
    expect(giveUp()).toBeNull();
  });

  it("offers nothing for a system server", async () => {
    calls.status.mockResolvedValue(stopped({ reaperExempt: true }));
    renderConsole();

    expect(await screen.findByText("Server is asleep")).toBeTruthy();
    expect(giveUp()).toBeNull();
    expect(del()).toBeNull();
  });

  it("shows a pending give-up and the way back in place of the card and the wake", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue(mine(true));
    calls.status.mockResolvedValue(stopped({ retiring: { requested_at: new Date().toISOString(), delete: false } }));
    renderConsole();

    expect(await screen.findByText("This server has been given up")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel request" })).toBeTruthy();
    expect(screen.getByText("Given up")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Wake" })).toBeNull();
    expect(giveUp()).toBeNull();
  });
});
