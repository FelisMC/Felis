// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { AuthSourcesPage } from "./AuthSourcesPage";
import type { AuthSourceConfig, AuthSourcesSettings } from "@/lib/types";

const calls = vi.hoisted(() => ({ getAuthSources: vi.fn(), setAuthSources: vi.fn(), testAuthSource: vi.fn() }));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: calls }));
const source = (tag = "littleskin", prefix = "LS"): AuthSourceConfig => ({ tag, prefix, url: "https://example.org/sessionserver/session/minecraft/hasJoined", api_url: "", enabled: true });
const settings = (sources = [source()]): AuthSourcesSettings => ({ sources, revision: "original", managed: false });
function page() { render(<MemoryRouter><AuthSourcesPage /></MemoryRouter>); }
const button = (name: string) => screen.getByRole("button", { name });
const field = (label: string, index = 0) => screen.getAllByLabelText(label)[index] as HTMLInputElement;
async function ready() { await userEvent.click(await screen.findByRole("button", { name: "Expand littleskin" })); }

beforeEach(() => {
  vi.clearAllMocks();
  calls.getAuthSources.mockResolvedValue(settings());
  calls.setAuthSources.mockImplementation(async (sources) => ({ sources, revision: "saved", managed: true }));
  calls.testAuthSource.mockResolvedValue({ ok: true, status: 204, elapsed_ms: 18 });
});

describe("AuthSourcesPage", () => {
  it("collapses saved sources and retains unsaved edits through keyboard folding", async () => {
    page();
    const toggle = await screen.findByRole("button", { name: "Expand littleskin" });
    expect(screen.queryByRole("textbox")).toBeNull();
    toggle.focus();
    await userEvent.keyboard("{Enter}");
    fireEvent.change(field("Name collision prefix"), { target: { value: "NEW" } });
    await userEvent.keyboard("{Enter}");
    expect(screen.queryByRole("textbox")).toBeNull();
    await userEvent.keyboard(" ");
    expect(screen.getByRole("textbox", { name: "Name collision prefix" })).toHaveProperty("value", "NEW");
    expect(calls.setAuthSources).not.toHaveBeenCalled();
  });
  it("toggles a closed source with the keyboard without expanding its fields", async () => {
    page();
    await screen.findByRole("button", { name: "Expand littleskin" });
    expect((button("Reload") as HTMLButtonElement).disabled).toBe(false);
    const enabled = screen.getByRole("switch", { name: "Enabled" });
    enabled.focus();
    await userEvent.keyboard(" ");
    expect(enabled.getAttribute("aria-checked")).toBe("false");
    expect(screen.queryByRole("textbox")).toBeNull();
    await userEvent.click(button("Save & apply"));
    await waitFor(() => expect(calls.setAuthSources).toHaveBeenCalledWith([{ ...source(), enabled: false }], "original"));
    expect(screen.getByRole("button", { name: "Expand littleskin" })).toBeTruthy();
  });
  it("shows disabled fields while loading, then protects saved IDs", async () => {
    let resolve!: (settings: AuthSourcesSettings) => void;
    calls.getAuthSources.mockReturnValue(new Promise((r) => { resolve = r; }));
    page();
    expect(field("Permanent source ID").disabled).toBe(true);
    expect(field("Authentication endpoint (hasJoined)").value).toBe("");
    expect((button("Save & apply") as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByRole("status").textContent).toContain("Loading authentication sources");
    await act(async () => resolve(settings()));
    await ready();
    expect(field("Permanent source ID").readOnly).toBe(true);
    expect(screen.queryByRole("button", { name: "Remove unsaved source" })).toBeNull();
  });
  it("adds a source, fixes a duplicate ID, saves and locks it without sending UI state", async () => {
    page(); await screen.findByRole("button", { name: "Expand littleskin" });
    await userEvent.click(button("Add source"));
    expect(screen.getByRole("button", { name: "Expand littleskin" }).getAttribute("aria-expanded")).toBe("false");
    expect(screen.getByRole("button", { name: "Collapse New source" }).getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("textbox", { name: "Permanent source ID" })).toBe(field("Permanent source ID", 1));
    fireEvent.change(field("Permanent source ID", 1), { target: { value: "littleskin" } });
    expect(field("Permanent source ID", 1).readOnly).toBe(false);
    expect(button("Remove unsaved source")).toBeTruthy();
    fireEvent.change(field("Permanent source ID", 1), { target: { value: "custom" } });
    fireEvent.change(field("Name collision prefix", 1), { target: { value: "CS" } });
    fireEvent.change(field("Authentication endpoint (hasJoined)", 1), { target: { value: "https://custom.example/check" } });
    fireEvent.change(field("Role lookup API root (optional)", 1), { target: { value: "https://custom.example" } });
    await userEvent.click(button("Save & apply"));
    await screen.findByText("Saved and active. New logins and role lookups use this configuration; online players stay connected.");
    expect(calls.setAuthSources).toHaveBeenCalledWith([source(), { tag: "custom", prefix: "CS", url: "https://custom.example/check", api_url: "https://custom.example", enabled: true }], "original");
    expect(field("Permanent source ID", 1).readOnly).toBe(true);
    expect(screen.queryByRole("button", { name: "Remove unsaved source" })).toBeNull();
    expect((button("Save & apply") as HTMLButtonElement).disabled).toBe(true);
  });
  it("reorders and disables a saved source, while discard restores its original fields and lock", async () => {
    calls.getAuthSources.mockResolvedValue(settings([source(), source("custom", "CS")]));
    page(); await ready();
    await userEvent.click(screen.getAllByRole("button", { name: "Move source up" })[1]);
    expect(screen.getByRole("button", { name: "Collapse littleskin" }).getAttribute("aria-controls")).toBe("source-fields-1");
    expect(screen.getByRole("button", { name: "Expand custom" }).getAttribute("aria-expanded")).toBe("false");
    expect(field("Permanent source ID").value).toBe("custom");
    await userEvent.click(screen.getAllByRole("switch", { name: "Enabled" })[0]);
    await userEvent.click(button("Save & apply"));
    await waitFor(() => expect(calls.setAuthSources).toHaveBeenCalledWith([{ ...source("custom", "CS"), enabled: false }, source()], "original"));
    await waitFor(() => expect((button("Save & apply") as HTMLButtonElement).disabled).toBe(true));
    await userEvent.click(button("Expand custom"));
    fireEvent.change(field("Name collision prefix"), { target: { value: "NEW" } });
    await userEvent.click(button("Discard changes"));
    expect(field("Name collision prefix").value).toBe("CS");
    expect(field("Permanent source ID").readOnly).toBe(true);
  });
  it("preserves a draft on conflict and lets the Owner discard then reload", async () => {
    calls.setAuthSources.mockRejectedValue({ status: 409, code: "auth_sources_changed" });
    page(); await ready();
    fireEvent.change(field("Name collision prefix"), { target: { value: "NEW" } });
    await userEvent.click(button("Save & apply"));
    await screen.findByRole("alert");
    expect(field("Name collision prefix").value).toBe("NEW");
    expect((button("Reload") as HTMLButtonElement).disabled).toBe(true);
    calls.getAuthSources.mockResolvedValue({ ...settings(), sources: [source("littleskin", "UP")], revision: "newest", managed: true });
    await userEvent.click(button("Discard changes"));
    await userEvent.click(button("Reload"));
    await waitFor(() => expect(field("Name collision prefix").value).toBe("UP"));
  });
  it("tests unsaved endpoints without saving, reports unexpected status and clears stale results on edit", async () => {
    page(); await ready();
    fireEvent.change(field("Authentication endpoint (hasJoined)"), { target: { value: "https://new.example/check" } });
    calls.testAuthSource.mockResolvedValueOnce({ ok: false, status: 200, elapsed_ms: 10 });
    await userEvent.click(button("Test connection"));
    await screen.findByText("Endpoint returned HTTP 200; expected 204 for an unused session.");
    expect(calls.testAuthSource).toHaveBeenLastCalledWith({ ...source(), url: "https://new.example/check" });
    expect(calls.setAuthSources).not.toHaveBeenCalled();
    fireEvent.change(field("Authentication endpoint (hasJoined)"), { target: { value: "https://valid.example/check" } });
    expect(screen.queryByRole("alert")).toBeNull();
    await userEvent.click(button("Test connection"));
    await screen.findByText("Authentication endpoint passed (18 ms).");
  });
  it("retries a failed initial load with the fields still visible and disabled", async () => {
    calls.getAuthSources.mockRejectedValueOnce({ status: 503, code: "auth_sources_unavailable" });
    page(); await screen.findByRole("alert");
    expect(field("Permanent source ID").disabled).toBe(true);
    await userEvent.click(button("Try again"));
    await ready();
    expect(field("Permanent source ID").value).toBe("littleskin");
  });
  it("supports installations with only the built-in source", async () => {
    calls.getAuthSources.mockResolvedValue(settings([]));
    page(); await screen.findByText("Only official accounts are enabled. Add a source to accept another provider.");
    expect(screen.queryByLabelText("Permanent source ID")).toBeNull();
    await userEvent.click(button("Add source"));
    expect(field("Permanent source ID").readOnly).toBe(false);
    expect((button("Save & apply") as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(button("Remove unsaved source"));
    expect((button("Save & apply") as HTMLButtonElement).disabled).toBe(true);
  });
});
