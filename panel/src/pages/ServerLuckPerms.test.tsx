// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { ServerLuckPerms } from "./ServerLuckPerms";

const calls = vi.hoisted(() => ({
  status: vi.fn(),
  myServers: vi.fn(),
  accessPlayers: vi.fn(),
  accessLuckPermsInfo: vi.fn(),
  accessGroup: vi.fn(),
}));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({
    loading: false,
    identity: { user_id: "admin-1", email: "admin@example.test", role: "admin" },
    isAdmin: true,
    isOwner: false,
  }),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const missing = { status: 409, code: "luckperms_missing", message: "LuckPerms is not installed on this server" };

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  calls.status.mockResolvedValue({ name: "lobby", displayName: "Lobby", phase: "Running" });
  calls.myServers.mockResolvedValue([]);
  calls.accessPlayers.mockResolvedValue({ online: 0, max: 20, players: [], output: "" });
  calls.accessLuckPermsInfo.mockRejectedValue(missing);
  calls.accessGroup.mockRejectedValue(missing);
});
afterEach(() => {
  vi.restoreAllMocks();
  return i18next.changeLanguage("en-US");
});

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/servers/lobby/luckperms"]}>
      <Routes>
        <Route path="/servers/:name/luckperms" element={<ServerLuckPerms />} />
      </Routes>
    </MemoryRouter>,
  );
}

// A server without LuckPerms (#4): the read of a player says why it came back
// empty, and a write is refused with the same cause and never lands in the
// history as a success.
describe("ServerLuckPerms without LuckPerms", () => {
  it("says LuckPerms is missing on the read and on a refused write", async () => {
    renderPage();
    await userEvent.type(await screen.findByPlaceholderText("Steve"), "Alex{Enter}");

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/LuckPerms isn't installed/);
    expect(calls.accessLuckPermsInfo).toHaveBeenCalledWith("lobby", "Alex");

    await userEvent.type(screen.getByLabelText("Group Name"), "vip");
    await userEvent.click(screen.getByRole("button", { name: /add parent group/i }));
    expect(calls.accessGroup).toHaveBeenCalledWith("lobby", "add", "Alex", "vip");
    const alerts = await screen.findAllByRole("alert");
    expect(alerts).toHaveLength(2);
    expect(alerts.every((a) => /LuckPerms isn't installed/.test(a.textContent ?? ""))).toBe(true);
    expect(screen.queryByText(/success/i)).toBeNull();
  });
});
