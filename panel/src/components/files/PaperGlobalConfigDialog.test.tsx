// @vitest-environment jsdom
import { StrictMode } from "react";
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { PaperGlobalConfigDialog } from "./PaperGlobalConfigDialog";
import type { PaperGlobalConfig } from "@/lib/types";

const calls = vi.hoisted(() => ({ read: vi.fn(), patch: vi.fn() }));
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, serverPaperGlobal: calls.read, patchServerPaperGlobal: calls.patch } };
});
beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const t = (key: string) => i18next.t(`files:${key}`);
const field = (id: string) => screen.getByRole("textbox", { name: t(`config_field_${id}`) }) as HTMLInputElement | HTMLTextAreaElement;
const save = () => screen.getByRole("button", { name: t("save") }) as HTMLButtonElement;
const response = (sha256 = "1".repeat(64)): PaperGlobalConfig => ({ sha256, values: {
  "chunk-loading-advanced.auto-config-send-distance": true,
  "chunk-loading-basic.player-max-chunk-send-rate": 75,
  "player-auto-save.max-per-tick": -1,
  "watchdog.early-warning-delay": 10000,
  "messages.no-permission": "Permission refused",
} });

beforeEach(() => {
  calls.read.mockReset().mockResolvedValue(response());
  calls.patch.mockReset().mockResolvedValue(response("3".repeat(64)));
});

function dialog(overrides: Partial<Parameters<typeof PaperGlobalConfigDialog>[0]> = {}) {
  const props = { name: "lobby", onClose: vi.fn(), onSaved: vi.fn(), beforeSave: vi.fn(), needsWorldConfirmation: false, ...overrides };
  const rendered = render(<PaperGlobalConfigDialog {...props} />);
  return { ...rendered, props };
}

describe("protected Paper global form", () => {
  it("shows only existing compatible whitelist values and never provides raw text or missing defaults", async () => {
    const original = response();
    original.values["watchdog.early-warning-delay"] = 9007199254740992;
    calls.read.mockResolvedValue({ ...original, values: { ...original.values, "proxies.velocity.secret": "fixture-secret" } });
    const { container } = dialog();
    await screen.findByRole("textbox", { name: t("config_field_global_no_permission") });
    expect(screen.getByText(t("paper_global_managed_hint"))).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: t("config_field_global_watchdog_delay") })).toBeNull();
    expect(screen.queryByRole("textbox", { name: t("config_field_global_concurrent_loads") })).toBeNull();
    expect(screen.queryByRole("radio", { name: t("properties_text") })).toBeNull();
    expect(screen.queryByRole("textbox", { name: t("file_content") })).toBeNull();
    expect(container.textContent).not.toContain("fixture-secret");
    expect(screen.queryByDisplayValue("fixture-secret")).toBeNull();
    expect(save().disabled).toBe(true);
    expect(calls.patch).not.toHaveBeenCalled();
  });

  it("sends only explicitly changed fields with the required original digest and leaves old out-of-range values alone", async () => {
    const original = response();
    original.values["watchdog.early-warning-delay"] = -2;
    calls.read.mockResolvedValue(original);
    const { props } = dialog();
    await screen.findByRole("textbox", { name: t("config_field_global_no_permission") });
    expect(field("global_watchdog_delay").value).toBe("-2");
    expect(screen.queryByRole("alert")).toBeNull();
    await userEvent.click(screen.getByRole("combobox", { name: t("config_field_global_auto_send") }));
    await userEvent.click(screen.getByRole("option", { name: t("properties_option_false") }));
    fireEvent.change(field("global_send_rate"), { target: { value: "12.5" } });
    fireEvent.keyDown(field("global_send_rate"), { key: "s", metaKey: true });
    await screen.findByRole("heading", { name: t("paper_global_title") });
    expect(calls.patch).toHaveBeenCalledExactlyOnceWith("lobby", {
      "chunk-loading-advanced.auto-config-send-distance": false, "chunk-loading-basic.player-max-chunk-send-rate": 12.5,
    }, "1".repeat(64));
    expect(props.beforeSave).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(props.onSaved).toHaveBeenCalledTimes(1));
  });

  it.each([
    ["global_autosave_budget", "-2", "0"],
    ["global_autosave_budget", "2147483648", "2147483647"],
    ["global_watchdog_delay", "-1", "0"],
    ["global_watchdog_delay", "1.5", "100"],
    ["global_send_rate", "-0.5", "0.5"],
    ["global_send_rate", "Infinity", "-1"],
    ["global_no_permission", "界".repeat(1366), "<red>Refused</red>\nSecond line"],
  ])("rejects invalid %s values and accepts corrected input", async (id, invalid, valid) => {
    dialog();
    await screen.findByRole("textbox", { name: t(`config_field_${id}`) });
    fireEvent.change(field(id), { target: { value: invalid } });
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(field(id).getAttribute("aria-invalid")).toBe("true");
    expect(save().disabled).toBe(true);
    fireEvent.keyDown(field(id), { key: "s", ctrlKey: true });
    expect(calls.patch).not.toHaveBeenCalled();
    fireEvent.change(field(id), { target: { value: valid } });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(save().disabled).toBe(false);
    await userEvent.click(save());
    expect(calls.patch).toHaveBeenCalledTimes(1);
  });

  it("retains changes after conflict and explicitly reloads the latest SHA before applying the same changes", async () => {
    calls.patch.mockRejectedValueOnce({ status: 409, code: "file_changed" }).mockResolvedValueOnce(response("3".repeat(64)));
    dialog();
    await screen.findByRole("textbox", { name: t("config_field_global_no_permission") });
    fireEvent.change(field("global_no_permission"), { target: { value: "My message" } });
    await userEvent.click(save());
    expect((await screen.findByRole("alert")).textContent).toContain(t("paper_global_conflict"));
    expect(field("global_no_permission").value).toBe("My message");
    expect(save().disabled).toBe(true);
    const latest = response("2".repeat(64));
    latest.values["player-auto-save.max-per-tick"] = 10;
    calls.read.mockResolvedValue(latest);
    await userEvent.click(screen.getByRole("button", { name: t("conflict_overwrite") }));
    expect(calls.read).toHaveBeenCalledTimes(2);
    expect(calls.patch.mock.calls).toEqual([
      ["lobby", { "messages.no-permission": "My message" }, "1".repeat(64)],
      ["lobby", { "messages.no-permission": "My message" }, "2".repeat(64)],
    ]);
  });

  it("loads latest by discarding edits rather than submitting a patch", async () => {
    calls.patch.mockRejectedValue({ status: 409, code: "file_changed" });
    dialog();
    await screen.findByRole("textbox", { name: t("config_field_global_no_permission") });
    fireEvent.change(field("global_no_permission"), { target: { value: "My message" } });
    await userEvent.click(save());
    await screen.findByText(t("paper_global_conflict"));
    const latest = response("2".repeat(64));
    latest.values["messages.no-permission"] = "External message";
    calls.read.mockResolvedValue(latest);
    await userEvent.click(screen.getByRole("button", { name: t("conflict_reload") }));
    expect(field("global_no_permission").value).toBe("External message");
    expect(save().disabled).toBe(true);
    expect(screen.queryByText(t("paper_global_conflict"))).toBeNull();
    expect(calls.patch).toHaveBeenCalledTimes(1);
  });

  it("keeps drafts and blocks patching until the parent world acknowledgement permits saving", async () => {
    const result = dialog({ needsWorldConfirmation: true });
    await screen.findByRole("textbox", { name: t("config_field_global_no_permission") });
    fireEvent.change(field("global_no_permission"), { target: { value: "Changed" } });
    fireEvent.keyDown(field("global_no_permission"), { key: "s", ctrlKey: true });
    expect(calls.patch).not.toHaveBeenCalled();
    expect(save().disabled).toBe(true);
    result.rerender(<PaperGlobalConfigDialog {...result.props} needsWorldConfirmation={false} />);
    await userEvent.click(save());
    expect(result.props.beforeSave).toHaveBeenCalledTimes(1);
    expect(calls.patch).toHaveBeenCalledTimes(1);
  });

  it("asks before discarding unsaved changes and guards browser navigation", async () => {
    const { props } = dialog();
    await screen.findByRole("textbox", { name: t("config_field_global_no_permission") });
    fireEvent.change(field("global_no_permission"), { target: { value: "Unsaved" } });
    const leave = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(leave);
    expect(leave.defaultPrevented).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: i18next.t("common:cancel") }));
    expect(screen.getByText(t("discard_prompt"))).toBeTruthy();
    expect(props.onClose).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: t("keep_editing") }));
    expect(field("global_no_permission").value).toBe("Unsaved");
    await userEvent.click(screen.getByRole("button", { name: i18next.t("common:cancel") }));
    await userEvent.click(screen.getByRole("button", { name: t("discard") }));
    expect(props.onClose).toHaveBeenCalledTimes(1);
    expect(calls.patch).not.toHaveBeenCalled();
  });

  it("rejects concurrent saves and disables controls during a request", async () => {
    let resolve!: (value: PaperGlobalConfig) => void;
    calls.patch.mockImplementation(() => new Promise<PaperGlobalConfig>((done) => { resolve = done; }));
    dialog();
    await screen.findByRole("textbox", { name: t("config_field_global_no_permission") });
    fireEvent.change(field("global_no_permission"), { target: { value: "Changed" } });
    const button = save();
    fireEvent.click(button);
    fireEvent.click(button);
    expect(calls.patch).toHaveBeenCalledTimes(1);
    expect(field("global_no_permission")).toHaveProperty("disabled", true);
    await act(async () => { resolve(response()); });
  });

  it("retains a draft while server state or maintenance disables editing", async () => {
    const result = dialog();
    await screen.findByRole("textbox", { name: t("config_field_global_no_permission") });
    fireEvent.change(field("global_no_permission"), { target: { value: "Changed" } });
    result.rerender(<PaperGlobalConfigDialog {...result.props} disabled />);
    expect(field("global_no_permission").value).toBe("Changed");
    expect(field("global_no_permission")).toHaveProperty("disabled", true);
    fireEvent.keyDown(field("global_no_permission"), { key: "s", ctrlKey: true });
    expect(calls.patch).not.toHaveBeenCalled();
    expect(screen.getByText(t("paper_global_unavailable"))).toBeTruthy();
  });

  it("survives StrictMode replay and ignores an older protected response", async () => {
    let resolve!: (value: PaperGlobalConfig) => void;
    calls.read.mockImplementationOnce(() => new Promise<PaperGlobalConfig>((done) => { resolve = done; })).mockResolvedValueOnce(response());
    render(<StrictMode><PaperGlobalConfigDialog name="lobby" onClose={vi.fn()} onSaved={vi.fn()} beforeSave={vi.fn()} needsWorldConfirmation={false} /></StrictMode>);
    await screen.findByRole("textbox", { name: t("config_field_global_no_permission") });
    const old = response("2".repeat(64));
    old.values["messages.no-permission"] = "Obsolete response";
    await act(async () => { resolve(old); });
    expect(field("global_no_permission").value).toBe("Permission refused");
    expect(calls.read).toHaveBeenCalledTimes(2);
  });
});
