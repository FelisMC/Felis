// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ServerConsole } from "./ServerConsole";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS } from "@/lib/hooks";
import { humanizeError } from "@/lib/api";

const calls = vi.hoisted(() => ({ status: vi.fn(), myServers: vi.fn(), listImages: vi.fn(), sendCommand: vi.fn() }));
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
  tier.isOwner = false;
  calls.status.mockReset();
  calls.myServers.mockReset();
  calls.myServers.mockResolvedValue([]);
  calls.listImages.mockReset();
  calls.listImages.mockResolvedValue([]);
  calls.sendCommand.mockReset();
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

describe("ServerConsole host recovery", () => {
  it("offers host recovery even when the first API read fails", async () => {
    tier.isOwner = true;
    calls.status.mockRejectedValue(new Error("API offline"));
    renderConsole();
    expect(await screen.findByText("Control API unreachable? Show host recovery steps")).toBeTruthy();
    expect(screen.getByText(/kubectl patch minecraftserver survival/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
    tier.isOwner = false;
  });
});

describe("ServerConsole failed start", () => {
  it("shows the failed attempt's log under what the owner can do once retries are spent", async () => {
    calls.status.mockResolvedValue(status({ desiredState: "Running", startGaveUp: true, autoRestarts: 3 }));
    renderConsole();

    const notice = await screen.findByRole("status", { name: "Server failed to start" });
    expect(within(notice).getByText(/automatic retry limit has been reached/)).toBeTruthy();
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

describe("ServerConsole doorways", () => {
  it("links to the server's scheduled tasks while it is stopped", async () => {
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped" }));
    renderConsole();

    const link = await screen.findByRole("link", { name: /^Scheduled tasks/ });
    expect(link.getAttribute("href")).toBe("/servers/survival/schedules");
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
    await waitFor(() => expect(screen.getByText(/Cannot confirm the current server state/)).toBeTruthy());
    expect(screen.getByText(/status backend down/)).toBeTruthy();
    expect(screen.getByTestId("log-stream")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
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

    const more = await screen.findByRole("button", { name: "More actions" });
    expect(giveUp()).toBeNull();
    await userEvent.click(more);
    expect(screen.getByRole("button", { name: "Give up" })).toBeTruthy();
    expect(del()).toBeNull();
  });

  it("offers an admin both", async () => {
    calls.status.mockResolvedValue(stopped());
    renderConsole();

    const more = await screen.findByRole("button", { name: "More actions" });
    expect(del()).toBeNull();
    await userEvent.click(more);
    expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy();
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

describe("ServerConsole command line", () => {
  const HISTORY = "felis:cmd:history:survival";

  beforeEach(() => {
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running", ready: true }));
    calls.sendCommand.mockResolvedValue({ output: "There are 0 of a max of 20 players online.\n" });
  });

  async function commandLine() {
    renderConsole();
    return (await screen.findByRole("textbox", { name: "Server command" })) as HTMLInputElement;
  }

  it("sends the typed command, trimmed, and shows the reply under it", async () => {
    const user = userEvent.setup();
    const box = await commandLine();

    await user.type(box, "  list  {Enter}");

    expect(calls.sendCommand).toHaveBeenCalledExactlyOnceWith("survival", "list");
    expect(await screen.findByText("There are 0 of a max of 20 players online.")).toBeTruthy();
    expect(screen.getByText(/^> list/)).toBeTruthy();
    expect(box.value).toBe("");
  });

  it("sends nothing for a blank line", async () => {
    const user = userEvent.setup();
    const box = await commandLine();

    await user.type(box, "   {Enter}");

    expect(calls.sendCommand).not.toHaveBeenCalled();
  });

  it("says why a command failed", async () => {
    const refusal = { status: 503, code: "rcon_unavailable", message: "rcon: connection refused" };
    calls.sendCommand.mockRejectedValue(refusal);
    const user = userEvent.setup();
    const box = await commandLine();

    await user.type(box, "list{Enter}");

    expect(await screen.findByText(humanizeError(refusal))).toBeTruthy();
  });

  it("sends nothing on the Enter that picks an input-method candidate", async () => {
    const box = await commandLine();

    fireEvent.compositionStart(box);
    fireEvent.change(box, { target: { value: "你好" } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect(calls.sendCommand).not.toHaveBeenCalled();

    fireEvent.compositionEnd(box);
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(calls.sendCommand).toHaveBeenCalledExactlyOnceWith("survival", "你好"));
  });

  it("walks the history with Up and Down, and brings back the line being typed", async () => {
    localStorage.setItem(HISTORY, JSON.stringify(["say hi", "list"]));
    const user = userEvent.setup();
    const box = await commandLine();

    await user.type(box, "tim");
    const seen: string[] = [];
    for (const key of ["{ArrowUp}", "{ArrowUp}", "{ArrowUp}", "{ArrowDown}", "{ArrowDown}", "{ArrowDown}"]) {
      await user.keyboard(key);
      seen.push(box.value);
    }

    expect(seen).toEqual(["list", "say hi", "say hi", "list", "tim", "tim"]);
  });

  it("leaves a fresh line alone on Down", async () => {
    localStorage.setItem(HISTORY, JSON.stringify(["say hi", "list"]));
    const user = userEvent.setup();
    const box = await commandLine();

    await user.keyboard("{ArrowDown}");

    expect(box.value).toBe("");
  });

  it("keeps each server's last 50 commands across visits, without repeats in a row", async () => {
    localStorage.setItem(HISTORY, JSON.stringify(Array.from({ length: 50 }, (_, i) => `cmd ${i}`)));
    const user = userEvent.setup();
    const box = await commandLine();

    await user.type(box, "list{Enter}");
    await waitFor(() => expect(calls.sendCommand).toHaveBeenCalledTimes(1));
    await user.type(box, "list{Enter}");
    await waitFor(() => expect(calls.sendCommand).toHaveBeenCalledTimes(2));

    const kept = JSON.parse(localStorage.getItem(HISTORY) ?? "[]") as string[];
    expect(kept).toHaveLength(50);
    expect(kept[0]).toBe("cmd 1");
    expect(kept.slice(-2)).toEqual(["cmd 49", "list"]);
  });
});

describe("startup diagnostics and stale state", () => {
  it("shows a crashed container even while the operator phase still says Starting", async () => {
    calls.status.mockResolvedValue(status({ phase: "Starting", desiredState: "Running", startup: { stage: "failed", reason: "OOMKilled", message: "Container minecraft exited with code 137", logsAvailable: false } }));
    renderConsole();
    expect(await screen.findByText("Startup issue")).toBeTruthy();
    expect(screen.getByText(/OOMKilled.*137/)).toBeTruthy();
    expect(screen.queryByText("Starting", { exact: true })).toBeNull();
    expect(screen.getByText("Logs are not available yet")).toBeTruthy();
  });
  it("explains scheduling failure before logs exist", async () => {
    calls.status.mockResolvedValue(status({ phase: "Starting", desiredState: "Running", startup: { stage: "scheduling", reason: "Unschedulable", message: "0/1 nodes: Insufficient memory", startedAt: new Date().toISOString(), logsAvailable: false } }));
    renderConsole();
    expect(await screen.findByText("Waiting for resources and scheduling")).toBeTruthy();
    expect(screen.getByText(/Not enough schedulable memory/)).toBeTruthy();
    expect(screen.queryByTestId("log-stream")).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete server" })).toBeNull();
    expect(screen.getByRole("button", { name: "More actions" })).toBeTruthy();
  });
  it("does not claim a live startup state when the API is unavailable, and recovers automatically", async () => {
    calls.status.mockResolvedValue(status({ phase: "Starting", desiredState: "Running", startup: { stage: "booting", logsAvailable: true } }));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderConsole();
    await screen.findByText("Starting Minecraft and checking readiness");
    try {
      calls.status.mockRejectedValue({ status: 0, code: "network_error", message: "backend unavailable" });
      await act(async () => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS));
      expect(screen.getByText("Status unavailable")).toBeTruthy();
      expect(screen.getByRole("alert").textContent).toContain("Last successful read");
      expect(screen.queryByText("Starting Minecraft and checking readiness")).toBeNull();
      expect(screen.getByTestId("log-stream")).toBeTruthy();
      expect(screen.queryByRole("button", { name: /^Stop/ })).toBeNull();
      calls.status.mockResolvedValue(status({ phase: "Running", ready: true, desiredState: "Running" }));
      await act(async () => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS));
      expect(screen.queryByText("Status unavailable")).toBeNull();
      expect(screen.getByTestId("log-stream")).toBeTruthy();
    } finally { vi.useRealTimers(); }
  });
});
