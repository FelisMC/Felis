// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { LobbyPage } from "./LobbyPage";

const calls = vi.hoisted(() => ({
  status: vi.fn(), restart: vi.fn(), listImages: vi.fn(), readServerFile: vi.fn(), writeServerFile: vi.fn(), createServerFile: vi.fn(), accessPermission: vi.fn(),
  nodeTasks: vi.fn(), nodeTask: vi.fn(), startNodeTask: vi.fn(),
}));
const tier = vi.hoisted(() => ({ isOwner: true }));
vi.mock("@/lib/tier", async (original) => ({
  ...await original<typeof import("@/lib/tier")>(),
  useTier: () => ({ loading: false, identity: { user_id: "staff-1", role: tier.isOwner ? "owner" : "admin" }, isAdmin: true, isOwner: tier.isOwner }),
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
  calls.nodeTasks.mockResolvedValue({ available: true, tasks: [] });
  tier.isOwner = true;
});

function page(space = "lobby") {
  render(<MemoryRouter initialEntries={[`/admin/lobby${space ? `?space=${space}` : ""}`]}><LobbyPage /></MemoryRouter>);
}

async function readyField(label: string) {
  const field = await screen.findByLabelText(label) as HTMLInputElement;
  await waitFor(() => expect(field.disabled).toBe(false));
  return field;
}

describe("LobbyPage", () => {
  it.each([["login", "Login book title"], ["lobby", "English menu title"]])("shows %s fields before either status or settings finish loading", async (space, label) => {
    let resolveRead!: (value: { content: string; sha256: string }) => void;
    let resolveStatus!: (value: object) => void;
    calls.readServerFile.mockReturnValue(new Promise((resolve) => { resolveRead = resolve; }));
    calls.status.mockReturnValue(new Promise((resolve) => { resolveStatus = resolve; }));
    page(space);
    const field = screen.getByLabelText(label) as HTMLInputElement;
    expect(field.value).toBe("");
    expect(field.disabled).toBe(true);
    expect(screen.getByText("Loading settings. Editing will be available once loaded.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save settings" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Restart & apply" }) as HTMLButtonElement).disabled).toBe(true);
    expect(calls.readServerFile).toHaveBeenCalledWith(space, "felis-experience.json");
    await act(async () => resolveRead({ content: btoa(JSON.stringify({ bookTitle: "My Login", menuTitleEn: "My Lobby" })), sha256: "read-hash" }));
    await readyField(label);
    expect(field.value).toBe(space === "login" ? "My Login" : "My Lobby");
    expect(screen.queryByText("Loading settings. Editing will be available once loaded.")).toBeNull();
    expect((screen.getByRole("button", { name: "Restart & apply" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => resolveStatus({ name: space, phase: "Running", desiredState: "Running", ready: true }));
    expect((screen.getByRole("button", { name: "Restart & apply" }) as HTMLButtonElement).disabled).toBe(false);
    expect(calls.writeServerFile).not.toHaveBeenCalled();
  });

  it("saves with the read hash and preserves unknown plugin settings", async () => {
    page();
    fireEvent.change(await readyField("English menu title"), { target: { value: "My Network" } });
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await screen.findByText("Saved. Settings apply on the next start or restart.");
    const [name, path, content, hash] = calls.writeServerFile.mock.calls[0];
    expect([name, path, hash]).toEqual(["lobby", "felis-experience.json", "read-hash"]);
    expect(JSON.parse(atob(content))).toEqual({ menuTitleEn: "My Network", customPlugin: { enabled: true } });
  });

  it("uses defaults for a missing file and creates without overwriting a concurrent file", async () => {
    calls.readServerFile.mockRejectedValue({ status: 404, code: "not_found" });
    page("login");
    fireEvent.change(await readyField("Login timeout (30–3600 seconds)"), { target: { value: "900" } });
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(calls.createServerFile).toHaveBeenCalled());
    expect(calls.createServerFile.mock.calls[0].slice(0, 2)).toEqual(["login", "felis-experience.json"]);
    expect(calls.writeServerFile).not.toHaveBeenCalled();
  });

  it("opens the leftmost login space by default", async () => {
    page("");
    await screen.findByLabelText("Login book title");
    expect(calls.status).toHaveBeenCalledWith("login");
    expect(screen.getByRole("tab", { name: "Login space" }).getAttribute("aria-selected")).toBe("true");
  });

  it("saves while running and restarts only when requested", async () => {
    calls.status.mockResolvedValue({ name: "lobby", phase: "Running", desiredState: "Running", ready: true, playersOnline: 0 });
    page();
    fireEvent.change(await readyField("English menu title"), { target: { value: "My Network" } });
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
    await readyField("English menu title");
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
    fireEvent.change(await readyField("English menu title"), { target: { value: "My Network" } });
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await screen.findByRole("alert");
    expect((screen.getByLabelText("English menu title") as HTMLInputElement).value).toBe("My Network");
    expect(calls.createServerFile).not.toHaveBeenCalled();
  });

  it("confirms a switch with unsaved edits", async () => {
    page();
    fireEvent.change(await readyField("English menu title"), { target: { value: "My Network" } });
    await userEvent.click(screen.getByRole("tab", { name: "Login space" }));
    await screen.findByRole("dialog", { name: "Discard unsaved settings?" });
    expect(calls.status).not.toHaveBeenCalledWith("login");
    await userEvent.click(screen.getByRole("button", { name: "Discard & switch" }));
    await screen.findByLabelText("Login book title");
    expect(calls.writeServerFile).not.toHaveBeenCalled();
  });

  // A file broken in the file manager must stay repairable here: the game runs on the
  // defaults, and saving writes a good file over the damaged one.
  it("opens a damaged file on the defaults and saves a good file over it", async () => {
    calls.readServerFile.mockResolvedValue({ content: btoa("{broken"), sha256: "damaged-hash" });
    page();
    expect((await screen.findByRole("alert")).textContent).toContain("felis-experience.json is damaged");
    // The form shows the defaults the game falls back to.
    expect((await readyField("English menu title")).value).toBe("Felis Servers");
    expect(screen.getByText("You have unsaved changes")).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(calls.writeServerFile).toHaveBeenCalledWith("lobby", "felis-experience.json", expect.any(String), "damaged-hash"));
    const written = JSON.parse(atob(calls.writeServerFile.mock.calls[0][2]));
    expect(written).toEqual({});
    await waitFor(() => expect(screen.queryByText(/is damaged/)).toBeNull());
  });

  it("keeps the still-valid settings of a damaged file", async () => {
    calls.readServerFile.mockResolvedValue({ content: btoa(JSON.stringify({ menuTitleEn: "Kept title", keepInventory: "yes" })), sha256: "damaged-hash" });
    page();
    expect((await screen.findByRole("alert")).textContent).toContain("is damaged");
    expect((await readyField("English menu title")).value).toBe("Kept title");
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(calls.writeServerFile).toHaveBeenCalled());
    expect(JSON.parse(atob(calls.writeServerFile.mock.calls[0][2]))).toEqual({ menuTitleEn: "Kept title" });
  });

  it("lets the Owner create a missing login space, then shows it", async () => {
    // Like the host: only the system-server pass (converge) creates the space, and it
    // exists once that task has finished.
    let converging = false;
    let created = false;
    calls.status.mockImplementation(async (name: string) => {
      if (!created) throw { status: 404, code: "not_found", message: "not found" };
      return { name, subdomain: name, phase: "Running", desiredState: "Running", ready: true, reaperExempt: true, playersOnline: 0, playersMax: 200 };
    });
    const task = (state: string) => ({ id: "task-1", state, stage: state === "running" ? "running" : "complete", startedAt: "2026-10-16T05:00:00Z", request: { action: "converge", params: { userRcon: "false" } }, log: "" });
    calls.startNodeTask.mockImplementation(async (r: { action: string }) => { converging = r.action === "converge"; return task("running"); });
    calls.nodeTask.mockImplementation(async () => { created = converging; return task("succeeded"); });
    page("login");
    expect(await screen.findByRole("heading", { name: "The login space does not exist" })).toBeTruthy();
    expect(screen.queryByLabelText("Login book title")).toBeNull();
    const create = await screen.findByRole("button", { name: "Create again" });
    await waitFor(() => expect((create as HTMLButtonElement).disabled).toBe(false));
    await userEvent.click(create);
    expect(await screen.findByLabelText("Login book title")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "The login space does not exist" })).toBeNull();
  });

  it("tells an Admin who can create a missing lobby", async () => {
    tier.isOwner = false;
    calls.status.mockRejectedValue({ status: 404, code: "not_found", message: "not found" });
    page("lobby");
    expect(await screen.findByRole("heading", { name: "The lobby does not exist" })).toBeTruthy();
    expect(screen.getByText(/Ask an Owner to create them again/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Create again" })).toBeNull();
    expect(calls.nodeTasks).not.toHaveBeenCalled();
  });

  it("keeps other status failures as an error with a retry", async () => {
    calls.status.mockRejectedValue({ status: 500, code: "internal", message: "boom" });
    page("login");
    expect((await screen.findByRole("alert")).textContent).toBe("The operation failed because of an internal server error. Ask an admin to check the logs.");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "The login space does not exist" })).toBeNull();
  });
});
