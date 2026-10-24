// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, beforeAll } from "vitest";
import { StrictMode, type ComponentProps } from "react";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { EditServerDialog } from "./EditServerDialog";
import { humanizeError } from "@/lib/api";
import type { ServerStartupSettings } from "@/lib/types";

const calls = vi.hoisted(() => ({ listImages: vi.fn(), patchServer: vi.fn(), patchMyServer: vi.fn(), getStartupSettings: vi.fn() }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

// Radix Select opens with pointer capture and scrolls the picked item into view,
// neither of which jsdom implements.
beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const onUpdated = vi.fn();
const t = (key: string, options: Record<string, string | number> = {}) => i18next.t(`servers:${key}`, options);
const startupValue = (): ServerStartupSettings => ({
  java_flags: ["-XX:+UseG1GC"],
  args: ["--message=hello world", "", "first\nsecond"],
  env: [{ name: "USER_MESSAGE", value: "  retained = value\nline two  " }],
  stop_commands: ["say Server closing", "stop"],
});
const itemName = (key: "java_flags" | "args" | "stop_commands", number: number) => t("edit_startup_item", { label: t(`edit_startup_${key}`), number });
const removeItem = (key: "java_flags" | "args" | "stop_commands", number = 1) => screen.getByRole("button", { name: t("edit_startup_remove_item", { label: t(`edit_startup_${key}`), number }) });
async function expandStartup(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: t("edit_startup_title") }));
  return await screen.findByLabelText(itemName("args", 1)) as HTMLTextAreaElement;
}

beforeEach(() => {
  calls.listImages.mockReset();
  calls.patchServer.mockReset();
  calls.patchMyServer.mockReset();
  calls.getStartupSettings.mockReset();
  onUpdated.mockReset();
  calls.listImages.mockResolvedValue([]);
  calls.patchServer.mockResolvedValue({ name: "survival", patched: [] });
  calls.patchMyServer.mockResolvedValue({ name: "survival", patched: [] });
  calls.getStartupSettings.mockResolvedValue(startupValue());
});

async function openDialog(overrides: Partial<ComponentProps<typeof EditServerDialog>> = {}) {
  const user = userEvent.setup();
  render(
    <EditServerDialog
      serverName="survival"
      currentDisplayName="Survival Realm"
      currentPolicy="ownerOnly"
      currentImage="registry.example.test/paper:1.21"
      currentMemory="4Gi"
      currentStorage="10Gi"
      currentCpu="1"
      currentIdleStopSeconds={600}
      {...overrides}
      onUpdated={onUpdated}
    />,
  );
  await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
  await screen.findByRole("dialog");
  return user;
}

const save = () => screen.getByRole("button", { name: "Save Config" }) as HTMLButtonElement;
const cpuInput = () => screen.getByLabelText("CPU Limit") as HTMLInputElement;

describe("EditServerDialog request body", () => {
  it("sends the CPU alone when only the CPU changed", async () => {
    const user = await openDialog();
    await user.clear(cpuInput());
    await user.type(cpuInput(), "2");
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { resources: { cpu: "2" } });
    expect(onUpdated).toHaveBeenCalledTimes(1);
  });

  it("sends the memory alone when only the memory changed", async () => {
    const user = await openDialog();
    await user.clear(screen.getByLabelText("Memory"));
    await user.type(screen.getByLabelText("Memory"), "8");
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { memory: "8Gi" });
  });

  it("takes a memory between whole GiB in MiB, and refuses one below 1 GiB", async () => {
    const user = await openDialog();
    await user.clear(screen.getByLabelText("Memory"));
    await user.type(screen.getByLabelText("Memory"), "0.5");
    expect(screen.getByText("Enter a number of at least 1, in GiB.")).toBeTruthy();
    expect(save().disabled).toBe(true);
    await user.clear(screen.getByLabelText("Memory"));
    await user.type(screen.getByLabelText("Memory"), "1.5");
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { memory: "1536Mi" });
  });

  it("clears the display name with an empty one", async () => {
    const user = await openDialog();
    await user.clear(screen.getByLabelText("Display name (optional)"));
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { displayName: "" });
  });

  it("stops the display name at the length the API takes", async () => {
    const user = await openDialog();
    await user.clear(screen.getByLabelText("Display name (optional)"));
    await user.paste("x".repeat(70));
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { displayName: "x".repeat(64) });
  });

  it("lifts the CPU limit when the field is emptied", async () => {
    const user = await openDialog();
    await user.clear(cpuInput());
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { resources: { cpu: "" } });
  });

  it("treats surrounding space as no change", async () => {
    const user = await openDialog();
    await user.type(cpuInput(), "  ");
    await user.type(screen.getByLabelText("Display name (optional)"), " ");

    expect(save().disabled).toBe(true);
  });

  it("refuses a CPU value the server cannot take", async () => {
    const user = await openDialog();
    for (const bad of ["two", "0", "1.5m", "-1"]) {
      await user.clear(cpuInput());
      await user.type(cpuInput(), bad);
      expect(screen.getByText("The CPU limit must be greater than zero, expressed as cores (for example, 1 or 1.5) or millicores (for example, 500m)."), bad).toBeTruthy();
      expect(cpuInput().getAttribute("aria-invalid"), bad).toBe("true");
      expect(save().disabled, bad).toBe(true);
    }
    for (const good of ["1.5", "500m", "4"]) {
      await user.clear(cpuInput());
      await user.type(cpuInput(), good);
      expect(screen.queryByText(/millicores/), good).toBeNull();
      expect(save().disabled, good).toBe(false);
    }
    expect(calls.patchServer).not.toHaveBeenCalled();
  });

  it("can be cancelled with nothing changed", async () => {
    const user = await openDialog();
    expect(save().disabled).toBe(true);
    const cancel = screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
    expect(cancel.disabled).toBe(false);
    await user.click(cancel);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("EditServerDialog private startup settings", () => {
  it("loads only on expansion and preserves empty and multiline arguments without making a change", async () => {
    const user = await openDialog();
    expect(calls.getStartupSettings).not.toHaveBeenCalled();
    expect(screen.queryByLabelText(itemName("args", 1))).toBeNull();
    await expandStartup(user);
    expect(calls.getStartupSettings).toHaveBeenCalledExactlyOnceWith("survival");
    expect(screen.getByLabelText(itemName("args", 1))).toHaveProperty("value", "--message=hello world");
    expect(screen.getByLabelText(itemName("args", 2))).toHaveProperty("value", "");
    expect(screen.getByLabelText(itemName("args", 3))).toHaveProperty("value", "first\nsecond");
    expect(screen.getByLabelText(t("edit_startup_env_value", { number: 1 }))).toHaveProperty("value", startupValue().env[0].value);
    expect(save().disabled).toBe(true);
    await user.click(screen.getByRole("button", { name: t("edit_startup_title") }));
    await expandStartup(user);
    expect(calls.getStartupSettings).toHaveBeenCalledTimes(1);
    expect(save().disabled).toBe(true);
  });

  it.each(["admin", "owner"] as const)("patches only the changed array for a server %s without splitting or trimming its entries", async (mode) => {
    const user = await openDialog({ mode, currentPhase: "Running" });
    await expandStartup(user);
    const value = "  one argument with spaces\nnext line  ";
    fireEvent.change(screen.getByLabelText(itemName("args", 2)), { target: { value } });
    expect(screen.getByText(t("edit_startup_restart_hint"))).toBeTruthy();
    await user.click(save());
    const expected = ["survival", { args: ["--message=hello world", value, "first\nsecond"] }];
    expect(mode === "admin" ? calls.patchServer : calls.patchMyServer).toHaveBeenCalledExactlyOnceWith(...expected);
    expect(mode === "admin" ? calls.patchMyServer : calls.patchServer).not.toHaveBeenCalled();
    expect(onUpdated).toHaveBeenCalledTimes(1);
  });

  it("sends empty arrays when their entries are explicitly removed", async () => {
    const user = await openDialog();
    await expandStartup(user);
    await user.click(removeItem("java_flags"));
    for (let at = 0; at < 3; at++) await user.click(removeItem("args"));
    for (let at = 0; at < 2; at++) await user.click(removeItem("stop_commands"));
    await user.click(screen.getByRole("button", { name: t("edit_startup_env_remove", { number: 1 }) }));
    await user.click(save());
    expect(calls.patchServer).toHaveBeenCalledExactlyOnceWith("survival", { java_flags: [], args: [], env: [], stop_commands: [] });
  });

  it("preserves untouched argument line endings when another entry is edited", async () => {
    calls.getStartupSettings.mockResolvedValue({ ...startupValue(), args: ["first\r\nsecond", "", "third\rfourth"] });
    const user = await openDialog();
    await expandStartup(user);
    fireEvent.change(screen.getByLabelText(itemName("args", 2)), { target: { value: "changed" } });
    await user.click(save());
    expect(calls.patchServer).toHaveBeenCalledExactlyOnceWith("survival", { args: ["first\r\nsecond", "changed", "third\rfourth"] });
  });

  it("keeps environment values literal and can add an empty value", async () => {
    const user = await openDialog();
    await expandStartup(user);
    const value = "  value = spaces\nand a newline  ";
    fireEvent.change(screen.getByLabelText(t("edit_startup_env_value", { number: 1 })), { target: { value } });
    await user.click(screen.getByRole("button", { name: t("edit_startup_env_add") }));
    await user.type(screen.getByLabelText(t("edit_startup_env_name", { number: 2 })), "USER_EMPTY");
    await user.click(save());
    expect(calls.patchServer).toHaveBeenCalledExactlyOnceWith("survival", { env: [{ name: "USER_MESSAGE", value }, { name: "USER_EMPTY", value: "" }] });
  });

  it("preserves stop-command order and explains that it applies at the next normal stop", async () => {
    const user = await openDialog({ currentPhase: "Running" });
    await expandStartup(user);
    fireEvent.change(screen.getByLabelText(itemName("stop_commands", 1)), { target: { value: "say First command" } });
    fireEvent.change(screen.getByLabelText(itemName("stop_commands", 2)), { target: { value: "say Second command" } });
    expect(screen.getByText(t("edit_startup_stop_commands_hint"))).toBeTruthy();
    expect(screen.queryByText(t("edit_startup_restart_hint"))).toBeNull();
    await user.click(save());
    expect(calls.patchServer).toHaveBeenCalledExactlyOnceWith("survival", { stop_commands: ["say First command", "say Second command"] });
  });

  it("keeps a private draft through status updates and when its fold is closed", async () => {
    const user = userEvent.setup();
    const rendered = render(<EditServerDialog serverName="survival" currentCpu="1" currentPhase="Starting" onUpdated={onUpdated} />);
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await expandStartup(user);
    fireEvent.change(screen.getByLabelText(itemName("args", 1)), { target: { value: "retained draft" } });
    rendered.rerender(<EditServerDialog serverName="survival" currentCpu="2" currentPhase="Running" onUpdated={onUpdated} />);
    expect(screen.getByLabelText(itemName("args", 1))).toHaveProperty("value", "retained draft");
    await user.click(screen.getByRole("button", { name: t("edit_startup_title") }));
    expect(screen.queryByLabelText(itemName("args", 1))).toBeNull();
    expect(screen.getByText(t("edit_startup_restart_hint"))).toBeTruthy();
    await user.click(save());
    expect(calls.patchServer).toHaveBeenCalledExactlyOnceWith("survival", { args: ["retained draft", "", "first\nsecond"] });
    expect(calls.getStartupSettings).toHaveBeenCalledTimes(1);
  });

  it("keeps fields unavailable after a failed read and retries without changing the base form", async () => {
    const failure = { status: 503, code: "upstream_unavailable" };
    calls.getStartupSettings.mockRejectedValueOnce(failure);
    const user = await openDialog();
    await user.click(screen.getByRole("button", { name: t("edit_startup_title") }));
    expect((await screen.findByRole("alert")).textContent).toBe(humanizeError(failure));
    expect(screen.queryByLabelText(itemName("args", 1))).toBeNull();
    expect(save().disabled).toBe(true);
    await user.click(screen.getByRole("button", { name: t("edit_startup_retry") }));
    await screen.findByLabelText(itemName("args", 1));
    expect(save().disabled).toBe(true);
    expect(calls.getStartupSettings).toHaveBeenCalledTimes(2);
    await user.clear(cpuInput());
    await user.type(cpuInput(), "2");
    await user.click(save());
    expect(calls.patchServer).toHaveBeenCalledExactlyOnceWith("survival", { resources: { cpu: "2" } });
  });

  it("preserves rejected settings and displays the backend validation error", async () => {
    const failure = { status: 400, code: "bad_startup_settings" };
    calls.patchServer.mockRejectedValueOnce(failure);
    const user = await openDialog();
    await expandStartup(user);
    fireEvent.change(screen.getByLabelText(itemName("java_flags", 1)), { target: { value: "-Xmx2G" } });
    await user.click(save());
    expect((await screen.findByRole("alert")).textContent).toBe(humanizeError(failure));
    expect(screen.getByLabelText(itemName("java_flags", 1))).toHaveProperty("value", "-Xmx2G");
    expect(save().disabled).toBe(false);
    expect(onUpdated).not.toHaveBeenCalled();
  });

  it("ignores a read from the previous opening, including under StrictMode", async () => {
    let resolve!: (value: ServerStartupSettings) => void;
    calls.getStartupSettings.mockImplementationOnce(() => new Promise<ServerStartupSettings>((done) => { resolve = done; }));
    const user = userEvent.setup();
    render(<StrictMode><EditServerDialog serverName="survival" onUpdated={onUpdated} /></StrictMode>);
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await user.click(screen.getByRole("button", { name: t("edit_startup_title") }));
    expect(screen.getByText(t("edit_startup_loading"))).toBeTruthy();
    expect(screen.queryByLabelText(itemName("args", 1))).toBeNull();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await expandStartup(user);
    await act(async () => { resolve({ ...startupValue(), args: ["obsolete response"] }); });
    expect(screen.getByLabelText(itemName("args", 1))).toHaveProperty("value", "--message=hello world");
    expect(calls.getStartupSettings).toHaveBeenCalledTimes(2);
    expect(save().disabled).toBe(true);
  });

  it("does not apply a pending read to a different server", async () => {
    let resolve!: (value: ServerStartupSettings) => void;
    calls.getStartupSettings.mockImplementationOnce(() => new Promise<ServerStartupSettings>((done) => { resolve = done; }));
    const user = userEvent.setup();
    const rendered = render(<EditServerDialog serverName="survival" onUpdated={onUpdated} />);
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await user.click(screen.getByRole("button", { name: t("edit_startup_title") }));
    rendered.rerender(<EditServerDialog serverName="second" onUpdated={onUpdated} />);
    expect(screen.queryByRole("dialog")).toBeNull();
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await expandStartup(user);
    await act(async () => { resolve({ ...startupValue(), args: ["first server response"] }); });
    expect(screen.getByLabelText(itemName("args", 1))).toHaveProperty("value", "--message=hello world");
    expect(calls.getStartupSettings.mock.calls).toEqual([["survival"], ["second"]]);
  });

  it("ignores the completion of a save after closing and reopening the dialog", async () => {
    let resolve!: (value: { name: string; patched: string[] }) => void;
    calls.patchServer.mockImplementationOnce(() => new Promise((done) => { resolve = done; }));
    const user = await openDialog();
    await expandStartup(user);
    fireEvent.change(screen.getByLabelText(itemName("args", 1)), { target: { value: "old submitted draft" } });
    await user.click(save());
    await waitFor(() => expect(calls.patchServer).toHaveBeenCalledTimes(1));
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await expandStartup(user);
    fireEvent.change(screen.getByLabelText(itemName("args", 1)), { target: { value: "new unsaved draft" } });
    await act(async () => { resolve({ name: "survival", patched: ["args"] }); });
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.getByLabelText(itemName("args", 1))).toHaveProperty("value", "new unsaved draft");
    expect(save().disabled).toBe(false);
    expect(onUpdated).not.toHaveBeenCalled();
  });

  it("does not apply a failed save to a different server", async () => {
    let reject!: (reason: unknown) => void;
    calls.patchServer.mockImplementationOnce(() => new Promise((_, fail) => { reject = fail; }));
    const user = userEvent.setup();
    const rendered = render(<EditServerDialog serverName="survival" onUpdated={onUpdated} />);
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await expandStartup(user);
    fireEvent.change(screen.getByLabelText(itemName("args", 1)), { target: { value: "old submitted draft" } });
    await user.click(save());
    rendered.rerender(<EditServerDialog serverName="second" onUpdated={onUpdated} />);
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await expandStartup(user);
    await act(async () => { reject({ status: 400, code: "bad_startup_settings" }); });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByLabelText(itemName("args", 1))).toHaveProperty("value", "--message=hello world");
    expect(save().disabled).toBe(true);
    expect(onUpdated).not.toHaveBeenCalled();
    expect(calls.patchServer).toHaveBeenCalledExactlyOnceWith("survival", { args: ["old submitted draft", "", "first\nsecond"] });
  });

  it("limits additional entries without rewriting an existing maximum-length array", async () => {
    calls.getStartupSettings.mockResolvedValue({ ...startupValue(), args: Array.from({ length: 128 }, () => "") });
    const user = await openDialog();
    await expandStartup(user);
    expect(screen.getByRole("button", { name: t("edit_startup_add_item", { label: t("edit_startup_args") }) })).toHaveProperty("disabled", true);
    expect(screen.getByLabelText(itemName("args", 128))).toHaveProperty("value", "");
    expect(save().disabled).toBe(true);
    await user.click(removeItem("args", 128));
    expect(screen.getByRole("button", { name: t("edit_startup_add_item", { label: t("edit_startup_args") }) })).toHaveProperty("disabled", false);
  });
});

describe("EditServerDialog recovery and mail", () => {
  it("sends only the recovery switch that changed and says what a crash then does", async () => {
    const user = await openDialog();
    const restart = screen.getByRole("switch", { name: /Restart after a crash/ });
    expect(restart.getAttribute("aria-checked")).toBe("true");
    expect((screen.getByLabelText("Restart at most") as HTMLInputElement).value).toBe("3");
    await user.click(restart);
    expect(screen.queryByLabelText("Restart at most")).toBeNull();
    expect(screen.getByText(/stays stopped until it is started by hand/)).toBeTruthy();
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { restartOnCrash: false });
  });

  it("turns resuming after a host restart off", async () => {
    const user = await openDialog();
    await user.click(screen.getByRole("switch", { name: /Resume after a host restart/ }));
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { resumeAfterHostRestart: false });
  });

  it("refuses a restart limit outside 1 to 20", async () => {
    const user = await openDialog();
    const limit = screen.getByLabelText("Restart at most") as HTMLInputElement;
    for (const bad of ["0", "21", "2.5", "x"]) {
      await user.clear(limit);
      await user.type(limit, bad);
      expect(screen.getByText("Enter a whole number from 1 to 20."), bad).toBeTruthy();
      expect(save().disabled, bad).toBe(true);
    }
    await user.clear(limit);
    await user.type(limit, "5");
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { maxCrashRestarts: 5 });
  });

  it("sends the mail switches that changed", async () => {
    const user = await openDialog();
    const problems = screen.getByRole("checkbox", { name: /Problems/ }) as HTMLInputElement;
    expect(problems.checked).toBe(true);
    await user.click(screen.getByRole("checkbox", { name: "Server stopped" }));
    await user.click(problems);
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { notify: { problems: false, stopped: true } });
  });

  it("shows a system service no recovery or mail settings", async () => {
    const user = userEvent.setup();
    render(<EditServerDialog serverName="lobby" systemService currentImage="registry.example.test/lobby:1" onUpdated={onUpdated} />);
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await screen.findByRole("dialog");
    expect(screen.queryByRole("switch", { name: /Restart after a crash/ })).toBeNull();
    expect(screen.queryByText("Email notifications")).toBeNull();
  });
});
