// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import i18next from "i18next";
import { ServerExtensions } from "./ServerExtensions";
import { humanizeError } from "@/lib/api";
import { STATUS_POLL_FAST_MS } from "@/lib/hooks";
import type { ServerExtension } from "@/lib/types";

const calls = vi.hoisted(() => ({
  status: vi.fn(), myServers: vi.fn(), listServerExtensions: vi.fn(), readServerFile: vi.fn(),
  renameServerFile: vi.fn(), deleteServerFile: vi.fn(), mkdirServerFolder: vi.fn(), uploadServerFile: vi.fn(),
  listServerFileOps: vi.fn(), serverJobs: vi.fn(),
}));
const tier = vi.hoisted(() => ({ isAdmin: true, loading: false }));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

// Radix Select requires DOM APIs that jsdom does not implement.
beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const t = (key: string, vars: Record<string, string | number> = {}) => i18next.t(`servers:${key}`, vars);
const fileText = (key: string) => i18next.t(`files:${key}`);
const stopped = { name: "demo", phase: "Stopped", desiredState: "Stopped", ready: false };
let entries: ServerExtension[];

function entry(over: Partial<ServerExtension> = {}): ServerExtension {
  return { path: "plugins/example.jar", filename: "example.jar", size: 2048, enabled: true, kind: "plugin", metadata_status: "ok",
    metadata: [{ id: "example", name: "Example Plugin", version: "1.2.3", authors: ["Test Author", "Test Author"], loaders: ["paper", "paper", "bukkit"] }],
    config_path: "plugins/Example #1", ...over };
}

function renderPage() {
  return render(<MemoryRouter initialEntries={["/servers/demo/extensions"]}><Routes>
    <Route path="/servers/:name/extensions" element={<ServerExtensions />} />
  </Routes></MemoryRouter>);
}

async function row(filename = "example.jar") {
  return screen.findByRole("article", { name: filename });
}

beforeEach(() => {
  for (const call of Object.values(calls)) call.mockReset();
  tier.isAdmin = true;
  tier.loading = false;
  entries = [entry()];
  calls.status.mockResolvedValue(stopped);
  calls.myServers.mockResolvedValue([]);
  calls.listServerExtensions.mockImplementation(async () => ({ entries, truncated: false }));
  calls.readServerFile.mockResolvedValue({ content: btoa("level-name=world\n") });
  calls.listServerFileOps.mockResolvedValue({ ops: [] });
  calls.serverJobs.mockResolvedValue([]);
  calls.mkdirServerFolder.mockResolvedValue({});
  calls.uploadServerFile.mockResolvedValue({});
  calls.renameServerFile.mockImplementation(async (_name: string, path: string, to: string) => {
    entries = entries.map((item) => item.path !== path ? item : { ...item, path: to, filename: to.split("/").pop()!, enabled: !to.endsWith(".disabled") });
    return {};
  });
  calls.deleteServerFile.mockImplementation(async (_name: string, path: string) => { entries = entries.filter((item) => item.path !== path); return {}; });
});
afterEach(() => { vi.useRealTimers(); });

describe("installed extensions", () => {
  it("requires ownership before reading installed metadata", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue([{ name: "demo", owned: false }]);
    renderPage();
    expect(await screen.findByText(t("extensions_not_yours_title"))).toBeTruthy();
    expect(calls.listServerExtensions).not.toHaveBeenCalled();
    expect(calls.readServerFile).not.toHaveBeenCalled();
  });

  it("allows a non-admin server owner to manage installed JAR files", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue([{ name: "demo", owned: true }]);
    renderPage();
    await userEvent.click(within(await row()).getByRole("button", { name: t("extensions_disable") }));
    expect(calls.renameServerFile).toHaveBeenCalledWith("demo", "plugins/example.jar", "plugins/example.jar.disabled");
  });

  it("renders all metadata from one JAR and deduplicates authors and loaders", async () => {
    entries = [entry({ metadata: [...entry().metadata, { id: "second", name: "Second Module", authors: [], loaders: ["forge", "neoforge"] }] })];
    renderPage();
    const article = within(await row());
    expect(article.getByText("Example Plugin")).toBeTruthy();
    expect(article.getByText("Second Module")).toBeTruthy();
    expect(article.getByText(t("extensions_version", { version: "1.2.3" }))).toBeTruthy();
    expect(article.getByText("Test Author")).toBeTruthy();
    expect(article.getByText("Paper, Bukkit")).toBeTruthy();
    expect(article.getByText("Forge, NeoForge")).toBeTruthy();
    expect(article.getByRole("link", { name: t("extensions_config") }).getAttribute("href")).toBe("/servers/demo/files?path=plugins%2FExample%20%231");
  });

  it("keeps unavailable metadata manageable and toggles the disabled suffix in both directions", async () => {
    entries = [entry({ metadata_status: "unavailable", metadata: [], config_path: undefined })];
    renderPage();
    expect(within(await row()).getByText(t("extensions_metadata_unavailable"))).toBeTruthy();
    await userEvent.click(within(await row()).getByRole("button", { name: t("extensions_disable") }));
    const disabled = await row("example.jar.disabled");
    expect(within(disabled).getByText(t("extensions_disabled"))).toBeTruthy();
    await userEvent.click(within(disabled).getByRole("button", { name: t("extensions_enable") }));
    await row();
    expect(calls.renameServerFile.mock.calls).toEqual([
      ["demo", "plugins/example.jar", "plugins/example.jar.disabled"], ["demo", "plugins/example.jar.disabled", "plugins/example.jar"],
    ]);
  });

  it("keeps the file and enabled state when renaming would overwrite an existing file", async () => {
    const conflict = { status: 409, code: "file_exists", message: "already there" };
    calls.renameServerFile.mockRejectedValue(conflict);
    renderPage();
    await userEvent.click(within(await row()).getByRole("button", { name: t("extensions_disable") }));
    expect(await screen.findByText(humanizeError(conflict))).toBeTruthy();
    expect(within(await row()).getByText(t("extensions_enabled"))).toBeTruthy();
    expect(screen.queryByRole("article", { name: "example.jar.disabled" })).toBeNull();
  });

  it("requires deletion confirmation while preserving the configuration directory", async () => {
    renderPage();
    await userEvent.click(within(await row()).getByRole("button", { name: t("extensions_delete_item", { filename: "example.jar" }) }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(t("extensions_delete_body"))).toBeTruthy();
    expect(calls.deleteServerFile).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: fileText("delete") }));
    await waitFor(() => expect(screen.queryByRole("article", { name: "example.jar" })).toBeNull());
    expect(calls.deleteServerFile.mock.calls).toEqual([["demo", "plugins/example.jar"]]);
  });

  it("refuses scans during startup and reads the list when the server becomes running", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.status.mockResolvedValue({ ...stopped, phase: "Starting", desiredState: "Running" });
    renderPage();
    expect(await screen.findByText(fileText("stopped_required_title"))).toBeTruthy();
    expect(calls.listServerExtensions).not.toHaveBeenCalled();
    calls.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running" });
    await act(async () => { await vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS); });
    expect(screen.getByRole("article", { name: "example.jar" })).toBeTruthy();
    expect(screen.getByText(t("extensions_restart_hint"))).toBeTruthy();
  });

  it("holds changes while a maintenance Job is using the world volume", async () => {
    calls.serverJobs.mockResolvedValue([{ kind: "restore", state: "running", name: "restore-demo" }]);
    renderPage();
    const article = within(await row());
    await screen.findByText(fileText("wait_for_restore"));
    expect((article.getByRole("button", { name: t("extensions_disable") }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: t("extensions_upload") }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("asks before a change when level-name puts the world in the plugins directory", async () => {
    calls.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running" });
    calls.readServerFile.mockResolvedValue({ content: btoa("level-name=plugins\n") });
    renderPage();
    await userEvent.click(within(await row()).getByRole("button", { name: t("extensions_disable") }));
    const ask = await screen.findByRole("dialog", { name: fileText("live_world_ask_title") });
    expect(calls.renameServerFile).not.toHaveBeenCalled();
    await userEvent.click(within(ask).getByRole("button", { name: fileText("live_world_ask_confirm") }));
    await waitFor(() => expect(calls.renameServerFile).toHaveBeenCalledWith("demo", "plugins/example.jar", "plugins/example.jar.disabled"));
  });

  it("uploads a batch into its selected directory and scans once after the queue drains", async () => {
    renderPage();
    await row();
    const scanned = calls.listServerExtensions.mock.calls.length;
    await userEvent.click(screen.getByRole("combobox", { name: t("extensions_upload_target") }));
    await userEvent.click(await screen.findByRole("option", { name: t("extensions_mods") }));
    fireEvent.change(screen.getByTestId("extension-upload-input"), { target: { files: [new File(["one"], "first.jar"), new File(["two"], "second.jar")] } });
    await waitFor(() => expect(calls.uploadServerFile).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getAllByText(fileText("upload_done"))).toHaveLength(2));
    expect(calls.uploadServerFile.mock.calls.map((call) => call[1])).toEqual(["mods/first.jar", "mods/second.jar"]);
    expect(calls.mkdirServerFolder).toHaveBeenCalledWith("demo", "mods");
    await waitFor(() => expect(calls.listServerExtensions).toHaveBeenCalledTimes(scanned + 1));
  });

  it("rejects non-JAR uploads before creating a directory or sending bytes", async () => {
    renderPage();
    await row();
    fireEvent.change(screen.getByTestId("extension-upload-input"), { target: { files: [new File(["bad"], "script.sh")] } });
    expect(await screen.findByText(t("extensions_jar_required"))).toBeTruthy();
    expect(calls.mkdirServerFolder).not.toHaveBeenCalled();
    expect(calls.uploadServerFile).not.toHaveBeenCalled();
  });
});
