// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { LobbyPage } from "./LobbyPage";

const calls = vi.hoisted(() => ({
  status: vi.fn(), restart: vi.fn(), listImages: vi.fn(), readServerFile: vi.fn(), writeServerFile: vi.fn(), createServerFile: vi.fn(), accessPermission: vi.fn(),
}));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: calls }));
vi.mock("@/lib/config", async (original) => ({ ...await original<typeof import("@/lib/config")>(), loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "example.test", gameVersion: "26.3" }) }));

beforeEach(() => {
  vi.clearAllMocks();
  calls.status.mockImplementation(async (name) => ({ name, subdomain: name, phase: "Stopped", desiredState: "Stopped", ready: false, reaperExempt: true, playersOnline: 0, playersMax: 200 }));
  calls.listImages.mockResolvedValue([]);
  calls.readServerFile.mockResolvedValue({ content: btoa(JSON.stringify({ menuTitleEn: "Old title", customPlugin: { enabled: true } })), sha256: "read-hash" });
  calls.writeServerFile.mockResolvedValue({ sha256: "saved-hash" });
  calls.createServerFile.mockResolvedValue({ sha256: "saved-hash" });
  calls.restart.mockResolvedValue({ name: "lobby", desiredState: "Running" });
});

function page(space = "lobby") {
  render(<MemoryRouter initialEntries={[`/admin/lobby${space ? `?space=${space}` : ""}`]}><LobbyPage /></MemoryRouter>);
}

describe("LobbyPage", () => {
  it("saves with the read hash and preserves unknown plugin settings", async () => {
    page();
    fireEvent.change(await screen.findByLabelText("English menu title"), { target: { value: "My Network" } });
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await screen.findByText("Saved. Settings apply on the next start or restart.");
    const [name, path, content, hash] = calls.writeServerFile.mock.calls[0];
    expect([name, path, hash]).toEqual(["lobby", "felis-experience.json", "read-hash"]);
    expect(JSON.parse(atob(content))).toEqual({ menuTitleEn: "My Network", customPlugin: { enabled: true } });
  });

  it("uses defaults for a missing file and creates without overwriting a concurrent file", async () => {
    calls.readServerFile.mockRejectedValue({ status: 404, code: "not_found" });
    page("login");
    fireEvent.change(await screen.findByLabelText("Login timeout (30–3600 seconds)"), { target: { value: "900" } });
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(calls.createServerFile).toHaveBeenCalled());
    expect(calls.createServerFile.mock.calls[0].slice(0, 2)).toEqual(["login", "felis-experience.json"]);
    expect(calls.writeServerFile).not.toHaveBeenCalled();
  });

  it("opens the leftmost login space by default", async () => {
    page("");
    await screen.findByLabelText("Login book title");
    expect(calls.status).toHaveBeenCalledWith("login");
    expect(screen.getByRole("button", { name: "Login space" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("saves while running and restarts only when requested", async () => {
    calls.status.mockResolvedValue({ name: "lobby", phase: "Running", desiredState: "Running", ready: true, playersOnline: 0 });
    page();
    fireEvent.change(await screen.findByLabelText("English menu title"), { target: { value: "My Network" } });
    expect((screen.getByRole("button", { name: "Restart & apply" }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await screen.findByText("Saved. Settings apply on the next start or restart.");
    expect(calls.restart).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Restart & apply" }));
    await waitFor(() => expect(calls.restart).toHaveBeenCalledWith("lobby"));
  });

  it("confirms a restart when players are online and keeps failures visible", async () => {
    calls.status.mockResolvedValue({ name: "lobby", phase: "Running", desiredState: "Running", ready: true, playersOnline: 2 });
    calls.restart.mockRejectedValue({ status: 409, code: "maintenance_in_progress", message: "busy" });
    page();
    await screen.findByLabelText("English menu title");
    await userEvent.click(screen.getByRole("button", { name: "Restart & apply" }));
    const dialog = await screen.findByRole("dialog", { name: "Restart this space?" });
    expect(calls.restart).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Restart & apply" }));
    await waitFor(() => expect(calls.restart).toHaveBeenCalledWith("lobby"));
    await screen.findByRole("alert");
    expect(screen.getByRole("dialog", { name: "Restart this space?" })).toBeTruthy();
  });

  it("keeps the draft when another editor changed the file", async () => {
    calls.writeServerFile.mockRejectedValue({ status: 409, code: "file_changed" });
    page();
    fireEvent.change(await screen.findByLabelText("English menu title"), { target: { value: "My Network" } });
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await screen.findByRole("alert");
    expect((screen.getByLabelText("English menu title") as HTMLInputElement).value).toBe("My Network");
    expect(calls.createServerFile).not.toHaveBeenCalled();
  });

  it("confirms a switch with unsaved edits", async () => {
    page();
    fireEvent.change(await screen.findByLabelText("English menu title"), { target: { value: "My Network" } });
    await userEvent.click(screen.getByRole("button", { name: "Login space" }));
    await screen.findByRole("dialog", { name: "Discard unsaved settings?" });
    expect(calls.status).not.toHaveBeenCalledWith("login");
    await userEvent.click(screen.getByRole("button", { name: "Discard & switch" }));
    await screen.findByLabelText("Login book title");
    expect(calls.writeServerFile).not.toHaveBeenCalled();
  });

  it("does not offer to save malformed JSON", async () => {
    calls.readServerFile.mockResolvedValue({ content: btoa("{broken"), sha256: "hash" });
    page();
    await screen.findByRole("alert");
    expect(screen.queryByRole("button", { name: "Save settings" })).toBeNull();
  });
});
