// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import type { Phase } from "@/lib/types";
import { PowerButton, SUBMITTED_HOLD_MS, SUBMITTED_RECHECK_MS } from "./PowerButton";

const { wake, stop } = vi.hoisted(() => ({ wake: vi.fn(), stop: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, wake, stop } };
});

const t = (key: string, opts?: Record<string, unknown>) => i18next.t(key, opts);

beforeEach(() => {
  wake.mockReset();
  stop.mockReset();
});

// A fake-timer test that fails must not leave the clock frozen for the rest.
afterEach(() => {
  vi.useRealTimers();
});

describe("PowerButton", () => {
  it("wakes a stopped server and tells the parent", async () => {
    wake.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" phase="Stopped" desiredState="Stopped" onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:wake") }));

    expect(wake).toHaveBeenCalledWith("lobby");
    expect(onChanged).toHaveBeenCalledOnce();
  });

  it("stays on the page with the reason when the wake is refused", async () => {
    wake.mockRejectedValue({ status: 429, code: "quota_exceeded", message: "raw" });
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" phase="Stopped" desiredState="Stopped" onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:wake") }));

    expect(await screen.findByRole("alert")).toHaveProperty("textContent", t("errors:quota_exceeded"));
    expect(onChanged).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: t("servers:wake") })).toHaveProperty("disabled", false);
  });

  it("stops an empty server without asking", async () => {
    stop.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" phase="Running" desiredState="Running" playersOnline={0} onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));

    expect(stop).toHaveBeenCalledWith("lobby");
    expect(onChanged).toHaveBeenCalledOnce();
  });

  it("asks before disconnecting players, and cancel sends nothing", async () => {
    render(<PowerButton name="lobby" phase="Running" desiredState="Running" playersOnline={3} onChanged={vi.fn()} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));
    expect(screen.getByText(t("servers:stop_confirm_players", { count: 3 }))).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: t("common:cancel") }));

    expect(stop).not.toHaveBeenCalled();
    expect(screen.queryByText(t("servers:stop_confirm_players", { count: 3 }))).toBeNull();
  });

  it("asks when the player count cannot be read", async () => {
    stop.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" phase="Running" desiredState="Running" playersOnline={0} playerCountUnknown onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));
    expect(stop).not.toHaveBeenCalled();
    expect(screen.getByText(t("servers:stop_confirm_unknown"))).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));

    expect(stop).toHaveBeenCalledWith("lobby");
    expect(onChanged).toHaveBeenCalledOnce();
  });

  it("keeps the confirmation open with the reason when the stop fails", async () => {
    stop.mockRejectedValue({ status: 409, code: "cooldown", message: "raw" });
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" phase="Running" desiredState="Running" playersOnline={2} onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));
    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));

    expect(await screen.findByText(t("errors:cooldown"))).toBeTruthy();
    expect(screen.getByText(t("servers:stop_confirm_players", { count: 2 }))).toBeTruthy();
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("sends one call however fast it is clicked", async () => {
    let resolve!: () => void;
    wake.mockReturnValue(new Promise<void>((r) => (resolve = r)));
    render(<PowerButton name="lobby" phase="Stopped" desiredState="Stopped" onChanged={vi.fn()} />);

    const button = screen.getByRole("button", { name: t("servers:wake") });
    await userEvent.click(button);
    await userEvent.click(screen.getByRole("button", { name: t("servers:waking") }));
    resolve();

    expect(wake).toHaveBeenCalledOnce();
  });

  it("offers a failed server a retry, which is the wake", async () => {
    wake.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" phase="Failed" desiredState="Running" failed onChanged={onChanged} />);

    expect(screen.queryByRole("button", { name: t("servers:wake") })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: t("servers:retry_start") }));

    expect(wake).toHaveBeenCalledWith("lobby");
    expect(stop).not.toHaveBeenCalled();
    expect(onChanged).toHaveBeenCalledOnce();
  });

  it("stops a failed server without asking, and only the pressed button spins", async () => {
    let resolve!: () => void;
    stop.mockReturnValue(new Promise<void>((r) => (resolve = r)));
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" phase="Failed" desiredState="Running" failed playerCountUnknown onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));

    expect(stop).toHaveBeenCalledWith("lobby");
    expect(screen.queryByText(t("servers:stop_confirm_unknown"))).toBeNull();
    expect(screen.getByRole("button", { name: t("servers:stopping") })).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: t("servers:retry_start") })).toHaveProperty("disabled", true);
    resolve();
    await vi.waitFor(() => expect(onChanged).toHaveBeenCalledOnce());
  });
});

describe("PowerButton after an accepted call", () => {
  const ok = () => wake.mockResolvedValue(undefined);

  it("keeps the wake in progress until the view shows it, then offers the stop", async () => {
    ok();
    const { rerender } = render(<PowerButton name="lobby" phase="Stopped" desiredState="Stopped" onChanged={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: t("servers:wake") }));

    // The parent's first reread can still carry the old status.
    rerender(<PowerButton name="lobby" phase="Stopped" desiredState="Stopped" onChanged={vi.fn()} />);
    expect(screen.getByRole("button", { name: t("servers:waking") })).toHaveProperty("disabled", true);
    expect(screen.queryByRole("button", { name: t("servers:wake") })).toBeNull();

    rerender(<PowerButton name="lobby" phase="Stopped" desiredState="Running" onChanged={vi.fn()} />);
    expect(screen.getByRole("button", { name: t("servers:stop") })).toHaveProperty("disabled", false);
  });

  it("asks the parent to reread until the view catches up, and no longer after", async () => {
    vi.useFakeTimers();
    ok();
    const onChanged = vi.fn();
    const { rerender } = render(
      <PowerButton name="lobby" phase="Stopped" desiredState="Stopped" onChanged={onChanged} />,
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: t("servers:wake") }));
    });
    const afterClick = onChanged.mock.calls.length;

    await act(() => vi.advanceTimersByTimeAsync(SUBMITTED_RECHECK_MS * 2));
    expect(onChanged.mock.calls.length).toBe(afterClick + 2);

    rerender(<PowerButton name="lobby" phase="Starting" desiredState="Running" onChanged={onChanged} />);
    const caughtUp = onChanged.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(SUBMITTED_RECHECK_MS * 3));
    expect(onChanged.mock.calls.length).toBe(caughtUp);
  });

  it("gives the button back when the view never catches up", async () => {
    vi.useFakeTimers();
    ok();
    render(<PowerButton name="lobby" phase="Stopped" desiredState="Stopped" onChanged={vi.fn()} />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: t("servers:wake") }));
    });
    expect(screen.getByRole("button", { name: t("servers:waking") })).toBeTruthy();

    await act(() => vi.advanceTimersByTimeAsync(SUBMITTED_HOLD_MS - 1));
    expect(screen.getByRole("button", { name: t("servers:waking") })).toBeTruthy();
    await act(() => vi.advanceTimersByTimeAsync(1));
    expect(screen.getByRole("button", { name: t("servers:wake") })).toHaveProperty("disabled", false);
  });

  it("keeps a stop in progress until the server is down", async () => {
    stop.mockResolvedValue(undefined);
    const { rerender } = render(
      <PowerButton name="lobby" phase="Running" desiredState="Running" playersOnline={0} onChanged={vi.fn()} />,
    );
    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));
    expect(screen.getByRole("button", { name: t("servers:stopping") })).toHaveProperty("disabled", true);

    rerender(<PowerButton name="lobby" phase="Running" desiredState="Stopped" onChanged={vi.fn()} />);
    expect(screen.getByRole("button", { name: t("servers:stopping") })).toHaveProperty("disabled", true);
    rerender(<PowerButton name="lobby" phase="Stopped" desiredState="Stopped" onChanged={vi.fn()} />);
    expect(screen.getByRole("button", { name: t("servers:wake") })).toHaveProperty("disabled", false);
  });

  it("keeps a retry in progress while the view still shows the failure", async () => {
    ok();
    const { rerender } = render(
      <PowerButton name="lobby" phase="Failed" desiredState="Running" failed onChanged={vi.fn()} />,
    );
    await userEvent.click(screen.getByRole("button", { name: t("servers:retry_start") }));

    rerender(<PowerButton name="lobby" phase="Failed" desiredState="Running" failed onChanged={vi.fn()} />);
    expect(screen.getByRole("button", { name: t("servers:waking") })).toHaveProperty("disabled", true);
    expect(screen.queryByRole("button", { name: t("servers:retry_start") })).toBeNull();

    rerender(<PowerButton name="lobby" phase="Starting" desiredState="Running" onChanged={vi.fn()} />);
    expect(screen.getByRole("button", { name: t("servers:stop") })).toBeTruthy();
  });
});

describe("PowerButton on a server already moving", () => {
  const cases: {
    phase: Phase;
    desiredState?: "Running" | "Stopped";
    shows: string;
    disabled: boolean;
  }[] = [
    { phase: "Stopping", shows: "servers:stopping", disabled: true },
    { phase: "Stopping", desiredState: "Stopped", shows: "servers:stopping", disabled: true },
    { phase: "Running", desiredState: "Stopped", shows: "servers:stopping", disabled: true },
    { phase: "Starting", desiredState: "Stopped", shows: "servers:stopping", disabled: true },
    // Woken while going down: it comes back up once it is down.
    { phase: "Stopping", desiredState: "Running", shows: "servers:waking", disabled: true },
    // Asked to run, no pod yet: Stop is the way out if it never comes up.
    { phase: "Stopped", desiredState: "Running", shows: "servers:stop", disabled: false },
    { phase: "Unknown", desiredState: "Running", shows: "servers:stop", disabled: false },
    { phase: "Starting", desiredState: "Running", shows: "servers:stop", disabled: false },
    // Without desiredState the phase alone decides.
    { phase: "Starting", shows: "servers:stop", disabled: false },
    { phase: "Unknown", shows: "servers:wake", disabled: false },
  ];

  for (const c of cases) {
    it(`${c.phase} asked ${c.desiredState ?? "(unknown)"} offers ${c.shows}`, () => {
      render(<PowerButton name="lobby" phase={c.phase} desiredState={c.desiredState} onChanged={vi.fn()} />);
      const buttons = screen.getAllByRole("button");
      expect(buttons).toHaveLength(1);
      expect(buttons[0].textContent).toBe(t(c.shows));
      expect(buttons[0]).toHaveProperty("disabled", c.disabled);
    });
  }
});

describe("PowerButton on a server given up or being deleted", () => {
  it("offers nothing to press, only the badge saying why", () => {
    const requested_at = new Date().toISOString();
    const { rerender } = render(
      <PowerButton name="lobby" phase="Stopped" desiredState="Stopped" retiring={{ requested_at, delete: false }} onChanged={vi.fn()} />,
    );
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText(t("servers:retiring_badge_release"))).toBeTruthy();

    // A failed start would otherwise offer its retry.
    rerender(
      <PowerButton name="lobby" phase="Failed" desiredState="Running" failed retiring={{ requested_at, delete: true }} onChanged={vi.fn()} />,
    );
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText(t("servers:retiring_badge_delete"))).toBeTruthy();
  });
});

describe("PowerButton on a page that needs the server down", () => {
  const cases: {
    phase: Phase;
    desiredState?: "Running" | "Stopped";
    failed?: boolean;
    shows: string;
    disabled: boolean;
  }[] = [
    { phase: "Running", desiredState: "Running", shows: "servers:stop", disabled: false },
    // A failed start gets Stop alone: a retry would start what the page waits to see down.
    { phase: "Failed", desiredState: "Running", failed: true, shows: "servers:stop", disabled: false },
    // Woken while going down: Stop keeps it down.
    { phase: "Stopping", desiredState: "Running", shows: "servers:stop", disabled: false },
    // A phase nobody can place offers Stop, never a wake.
    { phase: "Unknown", desiredState: "Stopped", shows: "servers:stop", disabled: false },
    { phase: "Unknown", shows: "servers:stop", disabled: false },
    { phase: "Failed", desiredState: "Stopped", shows: "servers:stop", disabled: false },
    // Already on its way down: nothing to press.
    { phase: "Stopping", desiredState: "Stopped", shows: "servers:stopping", disabled: true },
    { phase: "Running", desiredState: "Stopped", shows: "servers:stopping", disabled: true },
  ];

  for (const c of cases) {
    it(`${c.phase} asked ${c.desiredState ?? "(unknown)"}${c.failed ? " (failed)" : ""} offers ${c.shows}`, () => {
      render(
        <PowerButton name="lobby" phase={c.phase} desiredState={c.desiredState} failed={c.failed} stopOnly onChanged={vi.fn()} />,
      );
      const buttons = screen.getAllByRole("button");
      expect(buttons).toHaveLength(1);
      expect(buttons[0].textContent).toBe(t(c.shows));
      expect(buttons[0]).toHaveProperty("disabled", c.disabled);
    });
  }

  it("still asks before disconnecting players", async () => {
    stop.mockResolvedValue(undefined);
    render(<PowerButton name="lobby" phase="Running" desiredState="Running" playersOnline={2} stopOnly onChanged={vi.fn()} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));

    expect(stop).not.toHaveBeenCalled();
    expect(screen.getByText(t("servers:stop_confirm_players", { count: 2 }))).toBeTruthy();
  });
});
