// @vitest-environment jsdom
import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { ServerSchedules } from "./ServerSchedules";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS } from "@/lib/hooks";
import type { Schedule } from "@/lib/types";

const calls = vi.hoisted(() => ({
  status: vi.fn(),
  myServers: vi.fn(),
  listSchedules: vi.fn(),
  createSchedule: vi.fn(),
  updateSchedule: vi.fn(),
  deleteSchedule: vi.fn(),
  runSchedule: vi.fn(),
}));
const tier = vi.hoisted(() => ({
  loading: false,
  identity: { user_id: "admin-1", email: "admin@example.test", role: "admin" },
  isAdmin: true,
  isOwner: false,
}));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

// Radix Select opens with pointer capture and scrolls the picked item into view,
// neither of which jsdom implements.
beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const BROWSER = Intl.DateTimeFormat().resolvedOptions().timeZone;
const OTHER = BROWSER === "Pacific/Chatham" ? "Pacific/Kiritimati" : "Pacific/Chatham";
const KICK = "Online players are disconnected right away: a run started by hand skips the players' warning.";

function hoursFromNow(h: number): string {
  return new Date(Date.now() + h * 3600_000 + 60_000).toISOString();
}

function schedule(over: Partial<Schedule> = {}): Schedule {
  return {
    id: 1,
    server: "survival",
    label: "",
    action: "restart",
    command: "",
    every_minutes: 0,
    minute_of_day: 240,
    weekdays: 127,
    timezone: BROWSER,
    warn_minutes: 5,
    enabled: true,
    next_run_at: hoursFromNow(3),
    run_state: "",
    last_run_at: null,
    last_result: "",
    last_detail: "",
    created_by: "user:u1",
    created_at: "2026-09-01T00:00:00Z",
    ...over,
  };
}

function listing(schedules: Schedule[], limit = 20) {
  calls.listSchedules.mockResolvedValue({ schedules, limit });
}

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  tier.isAdmin = true;
  calls.status.mockResolvedValue({ name: "survival", displayName: "Survival", phase: "Running" });
  calls.myServers.mockResolvedValue([]);
  listing([]);
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  return i18next.changeLanguage("en-US");
});

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/servers/survival/schedules"]}>
      <Routes>
        <Route path="/servers/:name/schedules" element={<ServerSchedules />} />
      </Routes>
    </MemoryRouter>,
  );
}

async function rowOf(title: string): Promise<HTMLElement> {
  return (await screen.findByText(title, { selector: "p" })).closest("li") as HTMLElement;
}

async function openCreate() {
  await userEvent.click(await screen.findByRole("button", { name: "New task" }));
  return screen.findByRole("dialog");
}

async function pick(dialog: HTMLElement, box: string, option: string) {
  await userEvent.click(within(dialog).getByRole("combobox", { name: box }));
  await userEvent.click(await screen.findByRole("option", { name: option }));
}

describe("ServerSchedules", () => {
  it("shows a non-owner NotYours and never reads the list", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue([{ name: "survival", owned: false }]);
    renderPage();
    expect(await screen.findByText("No permission to manage scheduled tasks")).toBeTruthy();
    expect(calls.listSchedules).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "New task" })).toBeNull();
  });

  it("shows an owner who is not an admin the list", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue([{ name: "survival", owned: true }]);
    listing([schedule({ label: "Nightly" })]);
    renderPage();
    expect(await rowOf("Nightly")).toBeTruthy();
    expect(calls.listSchedules).toHaveBeenCalledWith("survival");
  });

  it("lists each task with its summary, next run and last result", async () => {
    listing([
      schedule({ id: 1, label: "Nightly", last_result: "ok", last_run_at: hoursFromNow(-22) }),
      schedule({
        id: 2,
        action: "command",
        command: "say hi",
        every_minutes: 30,
        minute_of_day: 0,
        weekdays: 62,
        timezone: OTHER,
        warn_minutes: 0,
        last_result: "failed",
        last_run_at: hoursFromNow(-2),
        last_detail: "the command failed: rcon: connection refused",
      }),
      schedule({ id: 3, action: "backup", weekdays: 42, minute_of_day: 330, warn_minutes: 0, enabled: false, next_run_at: null }),
    ]);
    renderPage();

    const nightly = await rowOf("Nightly");
    expect(within(nightly).getByText("Every day at 04:00")).toBeTruthy();
    expect(within(nightly).getByText("Next run in 3 hours")).toBeTruthy();
    expect(within(nightly).getByText("Succeeded")).toBeTruthy();
    expect(within(nightly).getByText("Last run 22 hours ago")).toBeTruthy();
    expect(within(nightly).getByText("Warns 5 min before")).toBeTruthy();
    expect(within(nightly).getByText("Restart")).toBeTruthy();
    expect(within(nightly).queryByText(BROWSER)).toBeNull();

    const command = await rowOf("Console command");
    expect(within(command).getByText("Every 30 minutes · Weekdays")).toBeTruthy();
    expect(within(command).getByText("say hi", { selector: "code" })).toBeTruthy();
    expect(within(command).getByText(OTHER)).toBeTruthy();
    expect(within(command).getByText("Failed")).toBeTruthy();
    expect(within(command).getByText("the command failed: rcon: connection refused")).toBeTruthy();
    expect(within(command).queryByText(/^Warns/)).toBeNull();

    const backup = await rowOf("Backup");
    expect(within(backup).getByText("Mon, Wed, Fri at 05:30")).toBeTruthy();
    expect(within(backup).getByText("Disabled; runs only when Run now is selected")).toBeTruthy();
    expect(within(backup).getByText("Off")).toBeTruthy();
    expect(within(backup).getByText("Not executed")).toBeTruthy();

    expect(screen.getByText("3 of 20 tasks")).toBeTruthy();
  });

  it("starts the week on Monday in Chinese", async () => {
    await i18next.changeLanguage("zh-CN");
    listing([schedule({ id: 1, label: "夜间重启", weekdays: 3 })]);
    renderPage();
    expect(within(await rowOf("夜间重启")).getByText("周一、周日 04:00")).toBeTruthy();
  });

  it("says in the panel's words why a run was skipped, and shows a command's reply", async () => {
    listing([
      schedule({
        id: 1,
        label: "Nightly",
        enabled: false,
        next_run_at: null,
        last_result: "skipped",
        last_run_at: hoursFromNow(-1),
        last_detail: "the server has a new owner since this schedule was saved; save it again to use it",
      }),
      schedule({
        id: 2,
        label: "Who",
        action: "command",
        command: "list",
        warn_minutes: 0,
        last_result: "ok",
        last_run_at: hoursFromNow(-1),
        last_detail: "There are 0 of a max of 20 players online",
      }),
    ]);
    renderPage();
    const nightly = await rowOf("Nightly");
    expect(within(nightly).getByText("Skipped")).toBeTruthy();
    expect(
      within(nightly).getByText(
        "The server owner has changed and the task was disabled automatically. Verify the configuration before saving or enabling it again.",
      ),
    ).toBeTruthy();
    expect(within(await rowOf("Who")).getByText("Reply: There are 0 of a max of 20 players online")).toBeTruthy();
  });

  it("creates a daily restart that warns the players", async () => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9, label: "Nightly" }));
    renderPage();
    const dialog = await openCreate();
    const restart = within(dialog).getByRole("radio", { name: "Restart" }) as HTMLInputElement;
    expect(restart.checked).toBe(true);
    // A command typed before switching back to a restart is not sent with it.
    await userEvent.click(within(dialog).getByRole("radio", { name: "Console command" }));
    await userEvent.type(within(dialog).getByLabelText("Command"), "say bye");
    await userEvent.click(restart);
    fireEvent.change(within(dialog).getByLabelText("Time"), { target: { value: "03:30" } });
    await pick(dialog, "Warn players", "10 minutes before");
    await userEvent.type(within(dialog).getByLabelText("Name (optional)"), "Nightly");
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));

    expect(calls.createSchedule).toHaveBeenCalledWith("survival", {
      label: "Nightly",
      action: "restart",
      command: "",
      every_minutes: 0,
      minute_of_day: 210,
      weekdays: 127,
      timezone: BROWSER,
      warn_minutes: 10,
      enabled: true,
    });
    expect(await screen.findByText("Created “Nightly”.")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(calls.listSchedules).toHaveBeenCalledTimes(2);
  });

  it("creates a repeating console command as typed, on weekdays", async () => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9, action: "command" }));
    renderPage();
    const dialog = await openCreate();
    await userEvent.click(within(dialog).getByRole("radio", { name: "Console command" }));
    await userEvent.type(within(dialog).getByLabelText("Command"), "/say hi");
    await userEvent.click(within(dialog).getByRole("radio", { name: "Repeat" }));
    await pick(dialog, "Repeat every", "30 minutes");
    await userEvent.click(within(dialog).getByRole("button", { name: "Weekdays" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));

    expect(calls.createSchedule).toHaveBeenCalledWith("survival", {
      label: "",
      action: "command",
      command: "/say hi",
      every_minutes: 30,
      minute_of_day: 0,
      weekdays: 62,
      timezone: BROWSER,
      warn_minutes: 0,
      enabled: true,
    });
  });

  it("drops the warning for a command and keeps a server action hourly at most", async () => {
    renderPage();
    const dialog = await openCreate();
    await pick(dialog, "Warn players", "10 minutes before");
    expect(within(dialog).getByRole("combobox", { name: "Warn players" }).textContent).toBe("10 minutes before");

    await userEvent.click(within(dialog).getByRole("radio", { name: "Console command" }));
    expect(within(dialog).queryByRole("combobox", { name: "Warn players" })).toBeNull();
    await userEvent.click(within(dialog).getByRole("radio", { name: "Repeat" }));
    await pick(dialog, "Repeat every", "15 minutes");

    await userEvent.click(within(dialog).getByRole("radio", { name: "Stop" }));
    expect(within(dialog).getByRole("combobox", { name: "Warn players" }).textContent).toBe("No warning");
    expect(within(dialog).getByRole("combobox", { name: "Repeat every" }).textContent).toBe("1 hour");
    await userEvent.click(within(dialog).getByRole("combobox", { name: "Repeat every" }));
    expect(await screen.findByRole("option", { name: "1 hour" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "30 minutes" })).toBeNull();
  });

  it("will not save a task on no day", async () => {
    renderPage();
    const dialog = await openCreate();
    for (const day of ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"]) {
      await userEvent.click(within(dialog).getByRole("button", { name: day }));
    }
    expect(within(dialog).getByText("Pick at least one day.")).toBeTruthy();
    const create = within(dialog).getByRole("button", { name: "Create" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    await userEvent.click(create);
    expect(calls.createSchedule).not.toHaveBeenCalled();

    await userEvent.click(within(dialog).getByRole("button", { name: "Wednesday" }));
    expect(within(dialog).queryByText("Pick at least one day.")).toBeNull();
    expect(create.disabled).toBe(false);
  });

  it("checks the command, name and time zone the way felis-api does", async () => {
    renderPage();
    const dialog = await openCreate();
    const create = within(dialog).getByRole("button", { name: "Create" }) as HTMLButtonElement;
    await userEvent.click(within(dialog).getByRole("radio", { name: "Console command" }));
    const command = within(dialog).getByLabelText("Command");
    expect(within(dialog).getByText("Enter a command.")).toBeTruthy();
    // A lone slash is dropped, leaving nothing to run.
    fireEvent.change(command, { target: { value: " / " } });
    expect(within(dialog).getByText("Enter a command.")).toBeTruthy();
    expect(create.disabled).toBe(true);
    // The limit counts the command after its slash goes.
    fireEvent.change(command, { target: { value: "/" + "x".repeat(1024) } });
    expect(create.disabled).toBe(false);
    fireEvent.change(command, { target: { value: "x".repeat(1025) } });
    expect(within(dialog).getByText("The command is longer than 1024 bytes.")).toBeTruthy();
    expect(create.disabled).toBe(true);
    // Bytes of UTF-8, as felis-api counts: 342 characters, 1026 bytes.
    fireEvent.change(command, { target: { value: "中".repeat(342) } });
    expect(within(dialog).getByText("The command is longer than 1024 bytes.")).toBeTruthy();
    fireEvent.change(command, { target: { value: "say hi" } });

    // Characters, not UTF-16 units, and the surrounding space does not count.
    const label = within(dialog).getByLabelText("Name (optional)");
    fireEvent.change(label, { target: { value: ` ${"𠀀".repeat(64)} ` } });
    expect(create.disabled).toBe(false);
    fireEvent.change(label, { target: { value: "𠀀".repeat(65) } });
    expect(within(dialog).getByText("The name is longer than 64 characters.")).toBeTruthy();
    expect(create.disabled).toBe(true);
    fireEvent.change(label, { target: { value: "" } });

    const zone = within(dialog).getByLabelText("Time zone") as HTMLInputElement;
    fireEvent.change(zone, { target: { value: "Mars/Olympus" } });
    expect(within(dialog).getByText("Unknown time zone. Use a name such as Asia/Shanghai.")).toBeTruthy();
    expect(create.disabled).toBe(true);
    fireEvent.change(zone, { target: { value: "  " } });
    expect(within(dialog).getByText("Enter a time zone.")).toBeTruthy();
    await userEvent.click(within(dialog).getByRole("button", { name: "Use the browser's zone" }));
    expect(zone.value).toBe(BROWSER);
    expect(create.disabled).toBe(false);
  });

  it("keeps the dialog open with the server's reason when a save is refused", async () => {
    calls.createSchedule.mockRejectedValue({
      status: 409,
      code: "schedule_limit",
      message: "a server can have at most 20 scheduled tasks",
    });
    renderPage();
    const dialog = await openCreate();
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "The server’s scheduled-task limit has been reached. Remove an existing task before retrying.",
    );
    expect(screen.getByRole("dialog")).toBe(dialog);
    expect((within(dialog).getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("starts a nightly backup from its template", async () => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9, action: "backup" }));
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Back up every night at 05:00" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/Backing up a running server first stops it/)).toBeTruthy();
    await userEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(calls.createSchedule).toHaveBeenCalledWith("survival", {
      label: "",
      action: "backup",
      command: "",
      every_minutes: 0,
      minute_of_day: 300,
      weekdays: 127,
      timezone: BROWSER,
      warn_minutes: 5,
      enabled: true,
    });
  });

  it("edits a task and saves the whole of it", async () => {
    listing([
      schedule({ id: 4, label: "Ad", action: "command", command: "say hi", every_minutes: 120, minute_of_day: 0, weekdays: 65, timezone: OTHER, warn_minutes: 0 }),
    ]);
    calls.updateSchedule.mockResolvedValue(schedule({ id: 4, label: "Advert" }));
    renderPage();
    await userEvent.click(within(await rowOf("Ad")).getByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Edit scheduled task")).toBeTruthy();
    expect(within(dialog).getByRole("combobox", { name: "Repeat every" }).textContent).toBe("2 hours");
    await userEvent.type(within(dialog).getByLabelText("Name (optional)"), "vert");
    await userEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(calls.updateSchedule).toHaveBeenCalledWith("survival", 4, {
      label: "Advert",
      action: "command",
      command: "say hi",
      every_minutes: 120,
      minute_of_day: 0,
      weekdays: 65,
      timezone: OTHER,
      warn_minutes: 0,
      enabled: true,
    });
    expect(await screen.findByText("Saved “Advert”.")).toBeTruthy();
  });

  it("switches a task off by saving all of it with enabled flipped", async () => {
    listing([
      schedule({ id: 4, label: "Nightly", minute_of_day: 330, weekdays: 42, timezone: OTHER, warn_minutes: 10 }),
    ]);
    let answer: (s: Schedule) => void = () => {};
    calls.updateSchedule.mockReturnValue(new Promise<Schedule>((resolve) => (answer = resolve)));
    renderPage();
    const toggle = (await screen.findByRole("switch", { name: "Run Nightly on schedule" })) as HTMLButtonElement;
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    await userEvent.click(toggle);
    expect(calls.updateSchedule).toHaveBeenCalledWith("survival", 4, {
      label: "Nightly",
      action: "restart",
      command: "",
      every_minutes: 0,
      minute_of_day: 330,
      weekdays: 42,
      timezone: OTHER,
      warn_minutes: 10,
      enabled: false,
    });
    expect(toggle.disabled).toBe(true);
    await act(async () => answer(schedule({ id: 4, enabled: false })));
    expect(await screen.findByText("“Nightly” is off.")).toBeTruthy();
  });

  it("asks before running a task now, and warns that a restart disconnects the players", async () => {
    listing([
      schedule({ id: 1, label: "Nightly" }),
      schedule({ id: 2, label: "Hello", action: "command", command: "say hi", warn_minutes: 0 }),
    ]);
    calls.runSchedule.mockResolvedValue(schedule({ id: 1, run_state: "stopping" }));
    renderPage();
    await userEvent.click(within(await rowOf("Nightly")).getByRole("button", { name: "Run now" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Stops the server now and starts it again.")).toBeTruthy();
    expect(within(dialog).getByText(KICK)).toBeTruthy();
    expect(calls.runSchedule).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Run now" }));
    expect(calls.runSchedule).toHaveBeenCalledWith("survival", 1);
    expect(await screen.findByText("Started “Nightly”.")).toBeTruthy();

    await userEvent.click(within(await rowOf("Hello")).getByRole("button", { name: "Run now" }));
    const second = await screen.findByRole("dialog");
    expect(within(second).getByText("say hi", { selector: "code" })).toBeTruthy();
    expect(within(second).queryByText(KICK)).toBeNull();
  });

  it("deletes a task once confirmed", async () => {
    listing([schedule({ id: 5, label: "Old" })]);
    calls.deleteSchedule.mockResolvedValue(null);
    renderPage();
    await userEvent.click(within(await rowOf("Old")).getByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Delete “Old”?")).toBeTruthy();
    expect(calls.deleteSchedule).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    expect(calls.deleteSchedule).toHaveBeenCalledWith("survival", 5);
    expect(await screen.findByText("Deleted “Old”.")).toBeTruthy();
  });

  it("locks a task while it runs, and says why", async () => {
    // A claim stamps last_run_at with the run's start and clears the result.
    const started = new Date(Date.now() - 5 * 60_000).toISOString();
    listing([
      schedule({ id: 1, label: "Busy", run_state: "stopping", last_run_at: started }),
      schedule({ id: 2, label: "Idle" }),
    ]);
    renderPage();
    const busy = await rowOf("Busy");
    expect(within(busy).getByText("Stopping the server…")).toBeTruthy();
    expect(within(busy).getByText("Started 5 minutes ago")).toBeTruthy();
    expect(within(busy).queryByText("Not executed")).toBeNull();
    for (const name of ["Run now", "Edit", "Delete"]) {
      const button = within(busy).getByRole("button", { name }) as HTMLButtonElement;
      expect(button.disabled).toBe(true);
      expect(button.parentElement?.title).toBe("The task is running. Wait for the current execution to complete.");
    }
    expect((within(busy).getByRole("switch") as HTMLButtonElement).disabled).toBe(true);

    const idle = await rowOf("Idle");
    for (const name of ["Run now", "Edit", "Delete"]) {
      expect((within(idle).getByRole("button", { name }) as HTMLButtonElement).disabled).toBe(false);
    }
    expect((within(idle).getByRole("switch") as HTMLButtonElement).disabled).toBe(false);
  });

  it("offers no new task once the server holds its limit", async () => {
    listing([schedule({ id: 1, label: "One" }), schedule({ id: 2, label: "Two" })], 2);
    renderPage();
    const add = (await screen.findByRole("button", { name: "New task" })) as HTMLButtonElement;
    await rowOf("One");
    expect(add.disabled).toBe(true);
    expect(add.parentElement?.title).toBe("The scheduled-task limit of 2 has been reached. Remove an existing task before adding another.");
  });

  it("says plainly when felis-api has no schedule store, and treats other failures as errors", async () => {
    calls.listSchedules.mockRejectedValue({ status: 503, code: "schedules_unavailable", message: "not configured" });
    const first = renderPage();
    expect(await screen.findByText("Scheduled tasks aren't available")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "New task" })).toBeNull();
    first.unmount();

    calls.listSchedules.mockRejectedValue({ status: 500, code: "internal", message: "boom" });
    renderPage();
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.queryByText("Scheduled tasks aren't available")).toBeNull();
  });

  it("rereads fast while a run is in progress", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    listing([schedule({ id: 1, label: "Backup", action: "backup", run_state: "backing_up" })]);
    renderPage();
    await screen.findByText("Backing up…");
    const reads = calls.listSchedules.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS + 100));
    expect(calls.listSchedules.mock.calls.length).toBe(reads + 1);
  });

  it("rereads slowly while nothing runs", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    listing([schedule({ id: 1, label: "Nightly" })]);
    renderPage();
    await rowOf("Nightly");
    const reads = calls.listSchedules.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS + 100));
    expect(calls.listSchedules.mock.calls.length).toBe(reads);
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_SLOW_MS - STATUS_POLL_FAST_MS));
    expect(calls.listSchedules.mock.calls.length).toBe(reads + 1);
  });
});
