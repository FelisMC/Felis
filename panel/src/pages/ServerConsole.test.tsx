// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";
import { ServerConsole } from "./ServerConsole";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS } from "@/lib/hooks";
import { humanizeError } from "@/lib/api";
import i18next from "i18next";
import type { ConsoleCommandResult } from "@/lib/types";

const calls = vi.hoisted(() => ({ status: vi.fn(), myServers: vi.fn(), listImages: vi.fn(), sendCommand: vi.fn(), serverUsage: vi.fn(), serverConsoleHistory: vi.fn() }));
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
// The live SSE log's connection is exercised separately.
vi.mock("@/components/LogConsole", async (importActual) => ({
  ...await importActual<typeof import("@/components/LogConsole")>(),
  LogConsole: () => <div data-testid="log-stream" />,
}));

beforeEach(() => {
  tier.isAdmin = true;
  tier.isOwner = false;
  calls.status.mockReset();
  calls.myServers.mockReset();
  calls.myServers.mockResolvedValue([]);
  calls.listImages.mockReset();
  calls.listImages.mockResolvedValue([]);
  calls.sendCommand.mockReset();
  calls.serverUsage.mockReset();
  calls.serverUsage.mockResolvedValue({ current: null, history: [] });
  calls.serverConsoleHistory.mockReset();
  calls.serverConsoleHistory.mockResolvedValue({ content: "", content_sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", has_more: false, byte_limited: false });
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
  it.each(["admin", "owner", "viewer"])("shows the extensions doorway only for a managing %s", async (role) => {
    tier.isAdmin = role === "admin";
    calls.myServers.mockResolvedValue([{ name: "survival", owned: role === "owner" }]);
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running" }));
    renderConsole();
    await screen.findByTestId("log-stream");
    const label = i18next.t("servers:extensions_title");
    if (role === "viewer") expect(screen.queryByText(label)).toBeNull();
    else expect((await screen.findByText(label)).closest("a")?.getAttribute("href")).toBe("/servers/survival/extensions");
  });
  it("does not fetch resource usage for a server the caller does not manage", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue([{ name: "survival", owned: false }]);
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running", disk: { usedBytes: 100, limitBytes: 1000, sampledAt: "2026-10-24T00:00:00Z", stale: false } }));
    renderConsole();
    await screen.findByTestId("log-stream");
    expect(screen.queryByLabelText("Resource usage")).toBeNull();
    expect(screen.queryByRole("region", { name: i18next.t("servers:disk_title") })).toBeNull();
    expect(calls.serverUsage).not.toHaveBeenCalled();
  });

  it("shows a stopped server's disk sample and refreshes its startup restriction", async () => {
    const disk = { usedBytes: 1200, limitBytes: 1000, sampledAt: "2026-10-24T00:00:00Z", stale: true };
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped", disk, diskFull: true }));
    renderConsole();
    const usage = await screen.findByRole("region", { name: i18next.t("servers:disk_title") });
    expect(within(usage).getByText("1.2 KiB / 1000 B")).toBeTruthy();
    expect(screen.getByRole("button", { name: i18next.t("servers:wake") })).toHaveProperty("disabled", true);
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped", disk: { ...disk, usedBytes: 100, stale: false }, diskFull: false }));
    fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(screen.getByRole("button", { name: i18next.t("servers:wake") })).toHaveProperty("disabled", false));
    expect(within(usage).getByText("100 B / 1000 B")).toBeTruthy();
  });
  it("offers host recovery even when the first API read fails", async () => {
    tier.isOwner = true;
    calls.status.mockRejectedValue(new Error("API offline"));
    renderConsole();
    expect(await screen.findByText("Host emergency shutdown procedure")).toBeTruthy();
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

  it("shows the saved log without failed-start actions once a failed server is stopped", async () => {
    calls.status.mockResolvedValue(status({ desiredState: "Stopped" }));
    renderConsole();

    expect(await screen.findByRole("region", { name: i18next.t("servers:log_history_title") })).toBeTruthy();
    expect(screen.queryByRole("status", { name: /failed to start|timed out/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /Retry start/ })).toBeNull();
  });
});

describe("ServerConsole edit dialog", () => {
  it.each(["admin", "owner", "viewer"])("shows extra ports and passes the current values only to a managing %s", async (role) => {
    tier.isAdmin = role === "admin";
    calls.myServers.mockResolvedValue([{ name: "survival", owned: role === "owner" }]);
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running", extraPorts: [{ protocol: "UDP", port: 24454, nodePort: 31001, address: "203.0.113.10:31001" }], extraPortsPending: true }));
    renderConsole();
    await screen.findByTestId("log-stream");
    const region = { name: i18next.t("servers:extra_ports_title") };
    if (role === "viewer") {
      expect(screen.queryByRole("region", region)).toBeNull();
      return;
    }
    const ports = await screen.findByRole("region", region);
    expect(within(ports).getByText("203.0.113.10:31001")).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    expect(screen.getByRole("spinbutton", { name: i18next.t("servers:extra_ports_internal", { number: 1 }) })).toHaveProperty("value", "24454");
    expect(within(screen.getByRole("dialog")).getByText(i18next.t("servers:extra_ports_pending"))).toBeTruthy();
  });

  it("offers the server's memory limit as the current memory, not the JVM heap", async () => {
    calls.status.mockResolvedValue(
      status({ phase: "Stopped", desiredState: "Stopped", memory: "4Gi", javaMemory: "3072M", cpu: "2" }),
    );
    renderConsole();

    await userEvent.setup().click(await screen.findByRole("button", { name: /Edit Server Config/ }));
    const memory = (await screen.findByLabelText("Memory")) as HTMLInputElement;
    expect(memory.value).toBe("4");
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

describe("ServerConsole saved logs", () => {
  afterEach(() => vi.useRealTimers());

  it.each([
    ["Stopped", "Stopped", false, true],
    ["Failed", "Stopped", false, true],
    ["Running", "Stopped", false, false],
    ["Starting", "Stopped", false, false],
    ["Stopping", "Stopped", false, false],
    ["Stopped", "Running", false, false],
    ["Stopped", "Stopped", true, false],
    ["Failed", "Running", false, false],
  ])("gates saved logs for phase %s, desired %s and readiness %s", async (phase, desiredState, ready, expected) => {
    calls.status.mockResolvedValue(status({ phase, desiredState, ready }));
    renderConsole();
    await screen.findByRole("heading", { name: "survival" });
    const region = { name: i18next.t("servers:log_history_title") };
    if (expected) {
      expect(await screen.findByRole("region", region)).toBeTruthy();
      expect(calls.serverConsoleHistory).toHaveBeenCalledExactlyOnceWith("survival", undefined);
      expect(screen.queryByRole("textbox")).toBeNull();
    } else {
      expect(screen.queryByRole("region", region)).toBeNull();
      expect(calls.serverConsoleHistory).not.toHaveBeenCalled();
    }
  });

  it.each(["admin", "owner", "viewer"])("permits saved log reads only for a managing %s", async (role) => {
    tier.isAdmin = role === "admin";
    calls.myServers.mockResolvedValue([{ name: "survival", owned: role === "owner" }]);
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped" }));
    renderConsole();
    await screen.findByRole("heading", { name: "survival" });
    if (role === "viewer") {
      await waitFor(() => expect(calls.myServers).toHaveBeenCalled());
      expect(calls.serverConsoleHistory).not.toHaveBeenCalled();
    } else {
      await screen.findByRole("region", { name: i18next.t("servers:log_history_title") });
      expect(calls.serverConsoleHistory).toHaveBeenCalledExactlyOnceWith("survival", undefined);
    }
  });

  it("does not poll saved logs and ignores a pending response after the server starts", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let resolve!: (page: { content: string; content_sha256: string; has_more: boolean; byte_limited: boolean }) => void;
    calls.serverConsoleHistory.mockImplementation(() => new Promise((done) => { resolve = done; }));
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped" }));
    renderConsole();
    await screen.findByRole("region", { name: i18next.t("servers:log_history_title") });
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_SLOW_MS * 3));
    expect(calls.serverConsoleHistory).toHaveBeenCalledTimes(1);
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running", ready: true }));
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_SLOW_MS));
    expect(await screen.findByTestId("log-stream")).toBeTruthy();
    await act(async () => { resolve({ content: btoa("late stopped log\n"), content_sha256: "a".repeat(64), has_more: false, byte_limited: false }); });
    expect(screen.queryByText("late stopped log")).toBeNull();
    expect(screen.queryByRole("region", { name: i18next.t("servers:log_history_title") })).toBeNull();
  });

  it("removes saved log access when cached server status becomes stale", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped" }));
    renderConsole();
    await screen.findByRole("region", { name: i18next.t("servers:log_history_title") });
    calls.status.mockRejectedValue({ code: "upstream", status: 503 });
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_SLOW_MS));
    await screen.findByText(/Cannot confirm the current server state/);
    expect(screen.queryByRole("region", { name: i18next.t("servers:log_history_title") })).toBeNull();
    expect(calls.serverConsoleHistory).toHaveBeenCalledTimes(1);
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
    expect(await screen.findByRole("region", { name: i18next.t("servers:log_history_title") })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Wake" })).toBeTruthy();

    // Nothing is due, so it rereads at the slow pace.
    calls.status.mockResolvedValue(status({ phase: "Running", desiredState: "Running", ready: true }));
    await advance(STATUS_POLL_SLOW_MS - 1_000);
    expect(calls.status).toHaveBeenCalledTimes(1);
    await advance(1_000);
    expect(await screen.findByTestId("log-stream")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: i18next.t("servers:log_history_title") })).toBeNull();
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
    expect(calls.serverConsoleHistory).not.toHaveBeenCalled();
  });

  it("offers nothing for a system server", async () => {
    calls.status.mockResolvedValue(stopped({ reaperExempt: true }));
    renderConsole();

    expect(await screen.findByRole("region", { name: i18next.t("servers:log_history_title") })).toBeTruthy();
    expect(giveUp()).toBeNull();
    expect(del()).toBeNull();
  });

  it("shows a pending give-up and the way back in place of the card and the wake", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue(mine(true));
    calls.status.mockResolvedValue(stopped({ retiring: { requested_at: new Date().toISOString(), delete: false } }));
    renderConsole();

    expect(await screen.findByText("Server pending reclamation")).toBeTruthy();
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
    calls.sendCommand.mockResolvedValue({ name: "survival", output: "There are 0 of a max of 20 players online.\n", transport: "rcon" });
    localStorage.clear();
  });
  afterEach(() => vi.useRealTimers());

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

  it("renders Minecraft and ANSI formatting in command replies without interpreting markup", async () => {
    calls.sendCommand.mockResolvedValue({ name: "survival", transport: "rcon", output: "§eHelp: §fIndex §lBold§r\n§x§1§2§3§4§5§6Hex\n\x1b[31m<b>unsafe</b>\x1b[0m" });
    const box = await commandLine();
    await userEvent.type(box, "help{Enter}");
    const help = await screen.findByText("Help:");
    expect(help.style.color).toBe("rgb(255, 255, 85)");
    expect(screen.getByText("Index").style.color).toBe("rgb(255, 255, 255)");
    expect(screen.getByText("Bold").style.fontWeight).toBe("700");
    expect(screen.getByText("Hex").style.color).toBe("rgb(18, 52, 86)");
    const unsafe = screen.getByText("<b>unsafe</b>");
    expect(unsafe.style.color).toBe("rgb(205, 49, 49)");
    expect(unsafe.closest("pre")?.querySelector("b")).toBeNull();
    expect(unsafe.closest("pre")?.textContent).not.toMatch(/§|\x1b/);
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
    expect(box.value).toBe("list");
  });

  it.each([
    ["Starting", "Running", false, true],
    ["Running", "Running", false, true],
    ["Running", "Running", true, true],
    ["Running", "Stopped", true, false],
    ["Stopped", "Running", false, false],
    ["Stopped", "Stopped", false, false],
    ["Stopping", "Running", false, false],
    ["Failed", "Running", false, false],
  ])("gates input for phase %s, desired %s and readiness %s", async (phase, desiredState, ready, expected) => {
    calls.status.mockResolvedValue(status({ phase, desiredState, ready, startup: phase === "Starting" ? { stage: "scheduling", logsAvailable: false } : undefined }));
    renderConsole();
    await screen.findByRole("heading", { name: "survival" });
    if (expected) expect(await screen.findByRole("textbox", { name: "Server command" })).toBeTruthy();
    else expect(screen.queryByRole("textbox", { name: "Server command" })).toBeNull();
    expect(calls.sendCommand).not.toHaveBeenCalled();
  });

  it.each(["admin", "owner", "viewer"])("allows command input only for a managing %s", async (role) => {
    tier.isAdmin = role === "admin";
    calls.myServers.mockResolvedValue([{ name: "survival", owned: role === "owner" }]);
    renderConsole();
    await screen.findByRole("heading", { name: "survival" });
    if (role === "viewer") {
      await waitFor(() => expect(calls.myServers).toHaveBeenCalled());
      expect(screen.queryByRole("textbox", { name: "Server command" })).toBeNull();
      expect(calls.sendCommand).not.toHaveBeenCalled();
    } else expect(await screen.findByRole("textbox", { name: "Server command" })).toBeTruthy();
  });

  it("submits during startup before logs are available and reports stdin submission without claiming a reply", async () => {
    calls.status.mockResolvedValue(status({ phase: "Starting", desiredState: "Running", ready: false, startup: { stage: "booting", logsAvailable: false } }));
    calls.sendCommand.mockResolvedValue({ name: "survival", output: "", transport: "stdin" });
    const box = await commandLine();
    expect(screen.getByText("Logs unavailable")).toBeTruthy();
    await userEvent.type(box, "say starting");
    await userEvent.click(screen.getByRole("button", { name: i18next.t("servers:command_send") }));
    expect(await screen.findByText(i18next.t("servers:command_sent_stdin"))).toBeTruthy();
    expect(screen.getByText(/^> say starting/)).toBeTruthy();
    expect(box.value).toBe("");
    expect(calls.sendCommand).toHaveBeenCalledExactlyOnceWith("survival", "say starting");
    expect(screen.queryByText("There are 0 of a max of 20 players online.")).toBeNull();
  });

  it("reports stdin submission while a running server has no RCON reply", async () => {
    calls.sendCommand.mockResolvedValue({ name: "survival", output: "", transport: "stdin" });
    const box = await commandLine();
    await userEvent.type(box, "list{Enter}");
    expect(await screen.findByText(i18next.t("servers:command_sent_stdin"))).toBeTruthy();
    expect(box.value).toBe("");
    expect(calls.sendCommand).toHaveBeenCalledTimes(1);
  });

  it("keeps an uncertain command visible and never resends it during status polling", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.sendCommand.mockRejectedValue({ status: 502, code: "command_outcome_unknown", message: "the command may have executed" });
    const box = await commandLine();
    fireEvent.change(box, { target: { value: "say uncertain" } });
    fireEvent.keyDown(box, { key: "Enter" });
    expect((await screen.findByRole("alert")).textContent).toBe(i18next.t("errors:command_outcome_unknown"));
    expect(box.value).toBe("say uncertain");
    expect(screen.getByText(/^> say uncertain/)).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_SLOW_MS * 2));
    expect(calls.sendCommand).toHaveBeenCalledExactlyOnceWith("survival", "say uncertain");
    expect(screen.queryByText(i18next.t("servers:command_sent_stdin"))).toBeNull();
  });

  it("keeps the pending command visible and refuses simultaneous or auto-repeated submissions", async () => {
    let resolve!: (result: ConsoleCommandResult) => void;
    calls.sendCommand.mockImplementation(() => new Promise<ConsoleCommandResult>((done) => { resolve = done; }));
    const box = await commandLine();
    fireEvent.change(box, { target: { value: "list" } });
    fireEvent.keyDown(box, { key: "Enter", repeat: true });
    expect(calls.sendCommand).not.toHaveBeenCalled();
    fireEvent.keyDown(box, { key: "Enter" });
    fireEvent.keyDown(box, { key: "Enter" });
    fireEvent.click(screen.getByRole("button", { name: i18next.t("servers:command_send") }));
    expect(calls.sendCommand).toHaveBeenCalledTimes(1);
    expect(box.value).toBe("list");
    expect(box.disabled).toBe(true);
    expect(screen.getByText(i18next.t("servers:command_sending"))).toBeTruthy();
    await act(async () => { resolve({ name: "survival", output: "Reply", transport: "rcon" }); });
    expect(await screen.findByText("Reply")).toBeTruthy();
    expect(box.disabled).toBe(false);
    expect(box.value).toBe("");
  });

  it("removes input when status becomes stale and ignores a pending reply", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let resolve!: (result: ConsoleCommandResult) => void;
    calls.sendCommand.mockImplementation(() => new Promise<ConsoleCommandResult>((done) => { resolve = done; }));
    const box = await commandLine();
    fireEvent.change(box, { target: { value: "list" } });
    fireEvent.keyDown(box, { key: "Enter" });
    calls.status.mockRejectedValue({ code: "upstream_unavailable", status: 503 });
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_SLOW_MS));
    await screen.findByText(/Cannot confirm the current server state/);
    expect(screen.queryByRole("textbox", { name: "Server command" })).toBeNull();
    await act(async () => { resolve({ name: "survival", output: "Stale reply", transport: "rcon" }); });
    expect(screen.queryByText("Stale reply")).toBeNull();
    expect(calls.sendCommand).toHaveBeenCalledTimes(1);
  });

  it("keeps late command replies out of the saved-log mode after a stop", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let resolve!: (result: ConsoleCommandResult) => void;
    calls.sendCommand.mockImplementation(() => new Promise<ConsoleCommandResult>((done) => { resolve = done; }));
    const box = await commandLine();
    fireEvent.change(box, { target: { value: "list" } });
    fireEvent.keyDown(box, { key: "Enter" });
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped", ready: false }));
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_SLOW_MS));
    expect(await screen.findByRole("region", { name: i18next.t("servers:log_history_title") })).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "Server command" })).toBeNull();
    await act(async () => { resolve({ name: "survival", output: "Late live reply", transport: "rcon" }); });
    expect(screen.queryByText("Late live reply")).toBeNull();
  });

  it("does not move an in-flight command or its result to another server", async () => {
    let resolve!: (result: ConsoleCommandResult) => void;
    calls.status.mockImplementation((name: string) => Promise.resolve(status({ name, phase: "Running", desiredState: "Running", ready: true })));
    calls.sendCommand.mockImplementation(() => new Promise<ConsoleCommandResult>((done) => { resolve = done; }));
    render(<MemoryRouter initialEntries={["/servers/survival"]}>
      <Link to="/servers/second">Switch server</Link>
      <Routes><Route path="/servers/:name" element={<ServerConsole />} /></Routes>
    </MemoryRouter>);
    const first = await screen.findByRole("textbox", { name: "Server command" });
    fireEvent.change(first, { target: { value: "say old server" } });
    fireEvent.keyDown(first, { key: "Enter" });
    await userEvent.click(screen.getByRole("link", { name: "Switch server" }));
    await screen.findByRole("heading", { name: "second" });
    const second = await screen.findByRole("textbox", { name: "Server command" });
    expect(second).toHaveProperty("value", "");
    await act(async () => { resolve({ name: "survival", output: "First server reply", transport: "rcon" }); });
    expect(screen.queryByText("First server reply")).toBeNull();
    expect(calls.sendCommand).toHaveBeenCalledExactlyOnceWith("survival", "say old server");
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
    expect(screen.getByText("Logs unavailable")).toBeTruthy();
  });
  it("explains scheduling failure before logs exist", async () => {
    calls.status.mockResolvedValue(status({ phase: "Starting", desiredState: "Running", startup: { stage: "scheduling", reason: "Unschedulable", message: "0/1 nodes: Insufficient memory", startedAt: new Date().toISOString(), logsAvailable: false } }));
    renderConsole();
    expect(await screen.findByText("Waiting for resources and scheduling")).toBeTruthy();
    expect(screen.getByText(/Insufficient schedulable memory/)).toBeTruthy();
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

describe("ServerConsole crashes and host restarts", () => {
  const ago = (min: number) => new Date(Date.now() - min * 60_000).toISOString();

  it("explains a server stopped for crashing too often, with the latest cause", async () => {
    calls.status.mockResolvedValue(status({
      phase: "Stopped", desiredState: "Stopped", maxCrashRestarts: 3, crashStopped: true,
      autoStop: { reason: "CrashLimit", at: ago(5) },
      lastCrash: { at: ago(5), reason: "OOMKilled", exitCode: 137 },
    }));
    renderConsole();

    const notice = await screen.findByRole("status", { name: "The server kept crashing and is no longer restarted" });
    expect(within(notice).getByText(/limit of 3 restarts/)).toBeTruthy();
    expect(within(notice).getByText(/Out of memory; the process was killed/)).toBeTruthy();
    expect(within(notice).getByText(/Players joining do not wake this server/)).toBeTruthy();
  });

  it("explains a server held back after a host restart", async () => {
    calls.status.mockResolvedValue(status({
      phase: "Stopped", desiredState: "Stopped", autoStop: { reason: "HostRestarted", at: ago(60) },
    }));
    renderConsole();

    const notice = await screen.findByRole("status", { name: "The server did not resume after a host restart" });
    expect(within(notice).getByText(/resuming after a host restart turned off/)).toBeTruthy();
  });

  it("says nothing about an idle stop or a stop that was started again", async () => {
    calls.status.mockResolvedValue(status({ phase: "Stopped", desiredState: "Stopped", autoStop: { reason: "Idle", at: ago(3) } }));
    renderConsole();
    await screen.findByRole("button", { name: /Edit Server Config/ });
    expect(screen.queryByRole("status", { name: /crash|host restart/ })).toBeNull();
  });

  it("counts recent crashes of a server that is running again", async () => {
    calls.status.mockResolvedValue(status({
      phase: "Running", desiredState: "Running", ready: true, restartOnCrash: true, maxCrashRestarts: 3,
      recentCrashes: 2, lastCrash: { at: ago(2), reason: "Error", exitCode: 1 },
    }));
    renderConsole();

    expect(await screen.findByText(/Crashed 2 times in a row within the last hour and was restarted each time \(limit 3\)/)).toBeTruthy();
    expect(screen.getByText(/exit code 1/)).toBeTruthy();
  });

  it("lists the latest events, newest first", async () => {
    calls.status.mockResolvedValue(status({
      phase: "Running", desiredState: "Running", ready: true,
      events: [
        { seq: 1, type: "HostRestarted", at: ago(90) },
        { seq: 2, type: "Started", at: ago(88) },
        { seq: 3, type: "Crashed", at: ago(10), reason: "Error", exitCode: 1, crashes: 1, limit: 3 },
      ],
    }));
    renderConsole();

    const list = within(await screen.findByRole("region", { name: "Activity" })).getAllByRole("listitem");
    expect(list.map((li) => li.textContent)).toEqual([
      expect.stringContaining("The process exited unexpectedly (exit code 1); restarted (crash 1 in a row, limit 3)"),
      expect.stringContaining("Server started"),
      expect.stringContaining("The host restarted; the server resumed"),
    ]);
  });
});
