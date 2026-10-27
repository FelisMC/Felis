// @vitest-environment jsdom
import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { ServerSchedules } from "./ServerSchedules";
import { STATUS_POLL_FAST_MS, STATUS_POLL_SLOW_MS } from "@/lib/hooks";
import type { Schedule, ScheduleStep } from "@/lib/types";

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
// 05:06 on 4 March 2099 in Shanghai (UTC+8, no daylight saving time).
const SHANGHAI_LOCAL = "2099-03-04T05:06";
const SHANGHAI_INSTANT = "2099-03-03T21:06:00.000Z";

const say = (command: string): ScheduleStep => ({ type: "command", command });

function hoursFromNow(h: number): string {
  return new Date(Date.now() + h * 3600_000 + 60_000).toISOString();
}

function schedule(over: Partial<Schedule> = {}): Schedule {
  return {
    id: 1,
    server: "survival",
    label: "",
    steps: [{ type: "restart" }],
    every_minutes: 0,
    minute_of_day: 240,
    weekdays: 127,
    timezone: BROWSER,
    run_once_at: null,
    max_runs: 0,
    run_count: 0,
    warn_minutes: 5,
    enabled: true,
    next_run_at: hoursFromNow(3),
    run_state: "",
    run_step: 0,
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

const click = (dialog: HTMLElement, role: "button" | "radio", name: string) =>
  userEvent.click(within(dialog).getByRole(role, { name }));

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
        steps: [say("say hi")],
        every_minutes: 30,
        minute_of_day: 0,
        weekdays: 62,
        timezone: OTHER,
        warn_minutes: 0,
        last_result: "failed",
        last_run_at: hoursFromNow(-2),
        last_detail: "the command failed: rcon: connection refused",
      }),
      schedule({ id: 3, steps: [{ type: "backup" }], weekdays: 42, minute_of_day: 330, warn_minutes: 0, enabled: false, next_run_at: null }),
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

  it("summarises the steps, the moment of a one-time task and the progress of a run limit", async () => {
    listing([
      schedule({ id: 1, label: "Reboot", steps: [say("say bye"), { type: "delay", seconds: 60 }, { type: "restart" }] }),
      schedule({ id: 2, steps: [{ type: "stop" }, { type: "backup" }, { type: "start" }] }),
      schedule({ id: 3, label: "Once", run_once_at: SHANGHAI_INSTANT, next_run_at: SHANGHAI_INSTANT, timezone: "Asia/Shanghai" }),
      schedule({ id: 4, label: "Limited", max_runs: 10, run_count: 3 }),
    ]);
    renderPage();
    const reboot = await rowOf("Reboot");
    expect(within(reboot).getByText("Console command → Delay 60 seconds → Restart")).toBeTruthy();
    expect(within(reboot).getByText("say bye", { selector: "code" })).toBeTruthy();
    expect(await rowOf("Stop → Backup → Start")).toBeTruthy();
    expect(within(await rowOf("Once")).getByText("Once on 2099-03-04 05:06")).toBeTruthy();
    expect(within(await rowOf("Limited")).getByText("Executed 3 of 10 times")).toBeTruthy();
  });

  it("starts the week on Monday in Chinese", async () => {
    await i18next.changeLanguage("zh-CN");
    listing([schedule({ id: 1, label: "夜间重启", weekdays: 3 })]);
    renderPage();
    expect(within(await rowOf("夜间重启")).getByText("周一、周日 04:00")).toBeTruthy();
  });

  it("says in the panel's words why a run was skipped or failed, and shows a command's reply", async () => {
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
        steps: [say("list")],
        warn_minutes: 0,
        last_result: "ok",
        last_run_at: hoursFromNow(-1),
        last_detail: "There are 0 of a max of 20 players online",
      }),
      schedule({
        id: 3,
        label: "Stuck",
        last_result: "failed",
        last_run_at: hoursFromNow(-1),
        last_detail: "the server did not stop within 15 minutes; the server was started again",
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
    expect(within(await rowOf("Stuck")).getByText("The server did not stop within 15 minutes. The server was started again.")).toBeTruthy();
  });

  it("creates a daily restart that warns the players", async () => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9, label: "Nightly" }));
    renderPage();
    const dialog = await openCreate();
    expect(within(dialog).getByRole("combobox", { name: "Type of step 1" }).textContent).toBe("Restart");
    // A command typed before switching back to a restart is not sent with it.
    await pick(dialog, "Type of step 1", "Console command");
    await userEvent.type(within(dialog).getByLabelText("Command of step 1"), "say bye");
    await pick(dialog, "Type of step 1", "Restart");
    fireEvent.change(within(dialog).getByLabelText("Time"), { target: { value: "03:30" } });
    await pick(dialog, "Warn players", "10 minutes before");
    await userEvent.type(within(dialog).getByLabelText("Name (optional)"), "Nightly");
    await click(dialog, "button", "Create");

    expect(calls.createSchedule).toHaveBeenCalledWith("survival", {
      label: "Nightly",
      steps: [{ type: "restart" }],
      every_minutes: 0,
      minute_of_day: 210,
      weekdays: 127,
      timezone: BROWSER,
      run_once_at: null,
      max_runs: 0,
      warn_minutes: 10,
      enabled: true,
    });
    expect(await screen.findByText("Created “Nightly”.")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(calls.listSchedules).toHaveBeenCalledTimes(2);
  });

  it("creates a repeating console command as typed, on weekdays", async () => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9, steps: [say("say hi")] }));
    renderPage();
    const dialog = await openCreate();
    await pick(dialog, "Type of step 1", "Console command");
    await userEvent.type(within(dialog).getByLabelText("Command of step 1"), "/say hi");
    await click(dialog, "radio", "Repeat");
    await pick(dialog, "Repeat every", "30 minutes");
    await click(dialog, "button", "Weekdays");
    await click(dialog, "button", "Create");

    expect(calls.createSchedule).toHaveBeenCalledWith("survival", {
      label: "",
      steps: [{ type: "command", command: "/say hi" }],
      every_minutes: 30,
      minute_of_day: 0,
      weekdays: 62,
      timezone: BROWSER,
      run_once_at: null,
      max_runs: 0,
      warn_minutes: 0,
      enabled: true,
    });
  });

  it("drops the warning for a command and offers only hourly or longer repeats beside a power step", async () => {
    renderPage();
    const dialog = await openCreate();
    await pick(dialog, "Warn players", "10 minutes before");
    expect(within(dialog).getByRole("combobox", { name: "Warn players" }).textContent).toBe("10 minutes before");

    await pick(dialog, "Type of step 1", "Console command");
    expect(within(dialog).queryByRole("combobox", { name: "Warn players" })).toBeNull();
    await click(dialog, "radio", "Repeat");
    await userEvent.click(within(dialog).getByRole("combobox", { name: "Repeat every" }));
    for (const minutes of ["1 minute", "5 minutes", "10 minutes", "15 minutes", "30 minutes"]) {
      expect(await screen.findByRole("option", { name: minutes })).toBeTruthy();
    }
    await userEvent.click(screen.getByRole("option", { name: "15 minutes" }));

    await pick(dialog, "Type of step 1", "Stop");
    expect(within(dialog).getByRole("combobox", { name: "Warn players" }).textContent).toBe("No warning");
    expect(within(dialog).getByRole("combobox", { name: "Repeat every" }).textContent).toBe("1 hour");
    await userEvent.click(within(dialog).getByRole("combobox", { name: "Repeat every" }));
    expect(await screen.findByRole("option", { name: "1 hour" })).toBeTruthy();
    for (const minutes of ["1 minute", "5 minutes", "10 minutes", "15 minutes", "30 minutes"]) {
      expect(screen.queryByRole("option", { name: minutes })).toBeNull();
    }
  });

  it("adds, moves and removes steps with the keyboard controls and sends them in order", async () => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9 }));
    renderPage();
    const dialog = await openCreate();
    expect((within(dialog).getByRole("button", { name: "Remove step 1" }) as HTMLButtonElement).disabled).toBe(true);

    await click(dialog, "button", "Add step");
    await userEvent.type(within(dialog).getByLabelText("Command of step 2"), "say hi");
    await click(dialog, "button", "Add step");
    await pick(dialog, "Type of step 3", "Delay");
    fireEvent.change(within(dialog).getByLabelText("Delay of step 3 (seconds)"), { target: { value: "30" } });
    expect((within(dialog).getByRole("button", { name: "Move step 1 up" }) as HTMLButtonElement).disabled).toBe(true);
    expect((within(dialog).getByRole("button", { name: "Move step 3 down" }) as HTMLButtonElement).disabled).toBe(true);

    await click(dialog, "button", "Move step 1 down");
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Move step 2 down" }));
    await click(dialog, "button", "Move step 2 down");
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Move step 3 up" }));
    expect((within(dialog).getByLabelText("Command of step 1") as HTMLInputElement).value).toBe("say hi");
    expect(within(dialog).getByRole("combobox", { name: "Type of step 3" }).textContent).toBe("Restart");

    await click(dialog, "button", "Add step");
    expect(within(dialog).getByRole("combobox", { name: "Type of step 4" })).toBeTruthy();
    await click(dialog, "button", "Remove step 4");
    expect(within(dialog).queryByRole("combobox", { name: "Type of step 4" })).toBeNull();

    await click(dialog, "button", "Create");
    expect(calls.createSchedule.mock.calls[0][1].steps).toEqual([
      { type: "command", command: "say hi" },
      { type: "delay", seconds: 30 },
      { type: "restart" },
    ]);
  });

  it("holds a task to ten steps", async () => {
    renderPage();
    const dialog = await openCreate();
    for (let i = 0; i < 9; i++) await click(dialog, "button", "Add step");
    expect(within(dialog).getAllByRole("combobox", { name: /^Type of step/ })).toHaveLength(10);
    const add = within(dialog).getByRole("button", { name: "Add step" }) as HTMLButtonElement;
    expect(add.disabled).toBe(true);
    expect(add.parentElement?.title).toBe("A task has at most 10 steps.");
  });

  it("fills the steps from the warn-then-restart template", async () => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9 }));
    renderPage();
    const dialog = await openCreate();
    await click(dialog, "button", "Warn, then restart");
    await click(dialog, "button", "Create");
    const steps = calls.createSchedule.mock.calls[0][1].steps as ScheduleStep[];
    expect(steps[0].type).toBe("command");
    expect(steps[0].command).toMatch(/^say /);
    expect(steps.slice(1)).toEqual([{ type: "delay", seconds: 60 }, { type: "restart" }]);
  });

  it.each([
    ["Stop, back up, start", [{ type: "stop" }, { type: "backup" }, { type: "start" }]],
    ["Save, then back up", [{ type: "command", command: "save-all" }, { type: "backup" }]],
  ])("fills the steps from the %s template", async (name, steps) => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9 }));
    renderPage();
    const dialog = await openCreate();
    await click(dialog, "button", name);
    await click(dialog, "button", "Create");
    expect(calls.createSchedule.mock.calls[0][1].steps).toEqual(steps);
  });

  it("checks the steps the way felis-api does", async () => {
    renderPage();
    const dialog = await openCreate();
    const create = within(dialog).getByRole("button", { name: "Create" }) as HTMLButtonElement;

    await pick(dialog, "Type of step 1", "Delay");
    expect(within(dialog).getByText("Add at least one step that is not a delay.")).toBeTruthy();
    expect(create.disabled).toBe(true);
    await click(dialog, "button", "Add step");
    fireEvent.change(within(dialog).getByLabelText("Command of step 2"), { target: { value: "say hi" } });
    expect(within(dialog).queryByText("Add at least one step that is not a delay.")).toBeNull();
    expect(create.disabled).toBe(false);

    const seconds = within(dialog).getByLabelText("Delay of step 1 (seconds)");
    for (const value of ["0", "3601", "1.5", ""]) {
      fireEvent.change(seconds, { target: { value } });
      expect(within(dialog).getByText("Enter a whole number of seconds from 1 to 3600.")).toBeTruthy();
      expect(create.disabled).toBe(true);
    }
    fireEvent.change(seconds, { target: { value: "3600" } });
    expect(create.disabled).toBe(false);

    await click(dialog, "button", "Add step");
    await pick(dialog, "Type of step 3", "Backup");
    await click(dialog, "button", "Add step");
    await pick(dialog, "Type of step 4", "Backup");
    expect(within(dialog).getByText("A task can have only one backup step.")).toBeTruthy();
    expect(create.disabled).toBe(true);
    await click(dialog, "button", "Remove step 4");
    expect(create.disabled).toBe(false);
  });

  it("will not save a task on no day", async () => {
    renderPage();
    const dialog = await openCreate();
    for (const day of ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"]) {
      await click(dialog, "button", day);
    }
    expect(within(dialog).getByText("Pick at least one day.")).toBeTruthy();
    const create = within(dialog).getByRole("button", { name: "Create" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    await userEvent.click(create);
    expect(calls.createSchedule).not.toHaveBeenCalled();

    await click(dialog, "button", "Wednesday");
    expect(within(dialog).queryByText("Pick at least one day.")).toBeNull();
    expect(create.disabled).toBe(false);
  });

  it("checks the command, name and time zone the way felis-api does", async () => {
    renderPage();
    const dialog = await openCreate();
    const create = within(dialog).getByRole("button", { name: "Create" }) as HTMLButtonElement;
    await pick(dialog, "Type of step 1", "Console command");
    const command = within(dialog).getByLabelText("Command of step 1");
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
    await click(dialog, "button", "Use the browser's zone");
    expect(zone.value).toBe(BROWSER);
    expect(create.disabled).toBe(false);
  });

  it.each([
    ["Asia/Shanghai", SHANGHAI_LOCAL, SHANGHAI_INSTANT],
    ["America/New_York", "2099-01-15T12:00", "2099-01-15T17:00:00.000Z"],
    ["America/New_York", "2099-07-15T12:00", "2099-07-15T16:00:00.000Z"],
  ])("reads a one-time task's date in %s as %s, which is the instant %s", async (zone, local, instant) => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9 }));
    renderPage();
    const dialog = await openCreate();
    await click(dialog, "radio", "One time only");
    expect(within(dialog).queryByRole("button", { name: "Monday" })).toBeNull();
    fireEvent.change(within(dialog).getByLabelText("Time zone"), { target: { value: zone } });
    fireEvent.change(within(dialog).getByLabelText("Run at"), { target: { value: local } });
    await click(dialog, "button", "Create");
    expect(calls.createSchedule).toHaveBeenCalledWith("survival", {
      label: "",
      steps: [{ type: "restart" }],
      every_minutes: 0,
      minute_of_day: 0,
      weekdays: 127,
      timezone: zone,
      run_once_at: instant,
      max_runs: 0,
      warn_minutes: 5,
      enabled: true,
    });
  });

  it("asks for the moment of a one-time task, which must lie ahead while the task is on", async () => {
    renderPage();
    const dialog = await openCreate();
    await click(dialog, "radio", "One time only");
    const create = within(dialog).getByRole("button", { name: "Create" }) as HTMLButtonElement;
    const runAt = within(dialog).getByLabelText("Run at");
    expect(within(dialog).getByText("Pick a date and time.")).toBeTruthy();
    expect(create.disabled).toBe(true);
    fireEvent.change(runAt, { target: { value: "2001-02-03T04:05" } });
    expect(within(dialog).getByText("Pick a time in the future.")).toBeTruthy();
    expect(create.disabled).toBe(true);
    await userEvent.click(within(dialog).getByRole("checkbox", { name: "Enabled" }));
    expect(create.disabled).toBe(false);
    await userEvent.click(within(dialog).getByRole("checkbox", { name: "Enabled" }));
    fireEvent.change(runAt, { target: { value: SHANGHAI_LOCAL } });
    expect(create.disabled).toBe(false);
  });

  it("edits a one-time task on the clock of its own zone and sends the same moment back", async () => {
    listing([schedule({ id: 4, label: "Once", run_once_at: "2099-03-03T21:06:00Z", timezone: "Asia/Shanghai", weekdays: 127, minute_of_day: 0 })]);
    calls.updateSchedule.mockResolvedValue(schedule({ id: 4, label: "Once" }));
    renderPage();
    await userEvent.click(within(await rowOf("Once")).getByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByRole("radio", { name: "One time only" }).getAttribute("aria-checked")).toBe("true");
    expect((within(dialog).getByLabelText("Run at") as HTMLInputElement).value).toBe(SHANGHAI_LOCAL);
    await click(dialog, "button", "Save");
    expect(calls.updateSchedule).toHaveBeenCalledWith("survival", 4, {
      label: "Once",
      steps: [{ type: "restart" }],
      every_minutes: 0,
      minute_of_day: 0,
      weekdays: 127,
      timezone: "Asia/Shanghai",
      run_once_at: SHANGHAI_INSTANT,
      max_runs: 0,
      warn_minutes: 5,
      enabled: true,
    });
  });

  it("sends a run limit, and refuses one the task has already used up", async () => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9 }));
    listing([schedule({ id: 4, label: "Limited", max_runs: 10, run_count: 3 })]);
    renderPage();
    const created = await openCreate();
    await userEvent.type(within(created).getByLabelText("Disable after N runs (optional)"), "10");
    await click(created, "button", "Create");
    expect(calls.createSchedule.mock.calls[0][1]).toMatchObject({ max_runs: 10 });

    await userEvent.click(within(await rowOf("Limited")).getByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog");
    const runs = within(dialog).getByLabelText("Disable after N runs (optional)") as HTMLInputElement;
    const save = within(dialog).getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(runs.value).toBe("10");
    fireEvent.change(runs, { target: { value: "3" } });
    expect(within(dialog).getByText("The task has already run 3 times. Enter a larger number, or leave the field empty.")).toBeTruthy();
    expect(save.disabled).toBe(true);
    fireEvent.change(runs, { target: { value: "10001" } });
    expect(within(dialog).getByText("Enter a whole number from 0 to 10000.")).toBeTruthy();
    expect(save.disabled).toBe(true);
    fireEvent.change(runs, { target: { value: "4" } });
    expect(save.disabled).toBe(false);
    fireEvent.change(runs, { target: { value: "" } });
    expect(save.disabled).toBe(false);
  });

  it("keeps the dialog open with the server's reason when a save is refused", async () => {
    calls.createSchedule.mockRejectedValue({
      status: 409,
      code: "schedule_limit",
      message: "a server can have at most 20 scheduled tasks",
    });
    renderPage();
    const dialog = await openCreate();
    await click(dialog, "button", "Create");
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "The server’s scheduled-task limit has been reached. Remove an existing task before retrying.",
    );
    expect(screen.getByRole("dialog")).toBe(dialog);
    expect((within(dialog).getByRole("button", { name: "Create" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("starts a nightly backup from its template", async () => {
    calls.createSchedule.mockResolvedValue(schedule({ id: 9, steps: [{ type: "backup" }] }));
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Back up every night at 05:00" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/A backup step backs up a running server/)).toBeTruthy();
    await click(dialog, "button", "Create");
    expect(calls.createSchedule).toHaveBeenCalledWith("survival", {
      label: "",
      steps: [{ type: "backup" }],
      every_minutes: 0,
      minute_of_day: 300,
      weekdays: 127,
      timezone: BROWSER,
      run_once_at: null,
      max_runs: 0,
      warn_minutes: 5,
      enabled: true,
    });
  });

  it("edits a task and saves the whole of it", async () => {
    listing([
      schedule({ id: 4, label: "Ad", steps: [say("say hi")], every_minutes: 120, minute_of_day: 0, weekdays: 65, timezone: OTHER, warn_minutes: 0 }),
    ]);
    calls.updateSchedule.mockResolvedValue(schedule({ id: 4, label: "Advert" }));
    renderPage();
    await userEvent.click(within(await rowOf("Ad")).getByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Edit scheduled task")).toBeTruthy();
    expect(within(dialog).getByRole("combobox", { name: "Repeat every" }).textContent).toBe("2 hours");
    await userEvent.type(within(dialog).getByLabelText("Name (optional)"), "vert");
    await click(dialog, "button", "Save");
    expect(calls.updateSchedule).toHaveBeenCalledWith("survival", 4, {
      label: "Advert",
      steps: [{ type: "command", command: "say hi" }],
      every_minutes: 120,
      minute_of_day: 0,
      weekdays: 65,
      timezone: OTHER,
      run_once_at: null,
      max_runs: 0,
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
      steps: [{ type: "restart" }],
      every_minutes: 0,
      minute_of_day: 330,
      weekdays: 42,
      timezone: OTHER,
      run_once_at: null,
      max_runs: 0,
      warn_minutes: 10,
      enabled: false,
    });
    expect(toggle.disabled).toBe(true);
    await act(async () => answer(schedule({ id: 4, enabled: false })));
    expect(await screen.findByText("“Nightly” is off.")).toBeTruthy();
  });

  it("keeps the exact moment of a one-time task when it is switched off", async () => {
    listing([schedule({ id: 4, label: "Soon", run_once_at: "2099-03-03T21:06:30Z", timezone: "Asia/Shanghai", minute_of_day: 0 })]);
    calls.updateSchedule.mockResolvedValue(schedule({ id: 4, enabled: false }));
    renderPage();
    await userEvent.click(await screen.findByRole("switch", { name: "Run Soon on schedule" }));
    expect(calls.updateSchedule.mock.calls[0][2]).toMatchObject({ run_once_at: "2099-03-03T21:06:30Z", enabled: false });
  });

  it("opens the editor instead of switching on a one-time task whose moment has passed", async () => {
    listing([schedule({ id: 4, label: "Past", run_once_at: "2001-02-03T04:05:00Z", timezone: "UTC", enabled: false, next_run_at: null, minute_of_day: 0 })]);
    renderPage();
    await userEvent.click(await screen.findByRole("switch", { name: "Run Past on schedule" }));
    expect(calls.updateSchedule).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Pick a time in the future.")).toBeTruthy();
    expect((within(dialog).getByRole("checkbox", { name: "Enabled" }) as HTMLInputElement).checked).toBe(true);
  });

  it("asks before running a task now, and warns that a restart disconnects the players", async () => {
    listing([
      schedule({ id: 1, label: "Nightly" }),
      schedule({ id: 2, label: "Hello", steps: [say("say hi")], warn_minutes: 0, max_runs: 5 }),
    ]);
    calls.runSchedule.mockResolvedValue(schedule({ id: 1, run_state: "stopping" }));
    renderPage();
    await userEvent.click(within(await rowOf("Nightly")).getByRole("button", { name: "Run now" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Carries out these steps now, in order: Restart.")).toBeTruthy();
    expect(within(dialog).getByText(KICK)).toBeTruthy();
    expect(within(dialog).queryByText(/counts toward the limit/)).toBeNull();
    expect(calls.runSchedule).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Run now" }));
    expect(calls.runSchedule).toHaveBeenCalledWith("survival", 1);
    expect(await screen.findByText("Started “Nightly”.")).toBeTruthy();

    await userEvent.click(within(await rowOf("Hello")).getByRole("button", { name: "Run now" }));
    const second = await screen.findByRole("dialog");
    expect(within(second).getByText("say hi", { selector: "code" })).toBeTruthy();
    expect(within(second).queryByText(KICK)).toBeNull();
    expect(within(second).getByText("This run counts toward the limit of 5 runs.")).toBeTruthy();
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
    expect(within(busy).getByText("Waiting for the server to stop…")).toBeTruthy();
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

  it("shows which step a run of several is at and what it waits for", async () => {
    const phases: [Schedule["run_state"], string][] = [
      ["claimed", "Running…"],
      ["delaying", "Waiting out the delay…"],
      ["stopping", "Waiting for the server to stop…"],
      ["backup_stopping", "Stopping the server for the backup…"],
      ["backup_waiting", "Waiting for the world to be free for the backup…"],
      ["backing_up", "Backing up…"],
      ["backup_restart", "Backing up; the server starts again afterwards…"],
      ["starting", "Starting the server…"],
      ["settling", "Waiting for the server to be ready…"],
    ];
    listing(
      phases.map(([state], i) =>
        schedule({ id: i + 1, label: `Task ${state}`, steps: [say("say hi"), { type: "restart" }], run_state: state, run_step: 1, last_run_at: hoursFromNow(-1) }),
      ),
    );
    renderPage();
    for (const [state, text] of phases) {
      expect(within(await rowOf(`Task ${state}`)).getByText(`Step 2 of 2: ${text}`)).toBeTruthy();
    }
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
    listing([schedule({ id: 1, label: "Backup", steps: [{ type: "backup" }], run_state: "backing_up" })]);
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
