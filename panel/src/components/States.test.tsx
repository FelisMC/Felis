// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NotRunning, RefreshError } from "./States";
import { humanizeError } from "@/lib/api";
import type { Phase } from "@/lib/types";
import type { StartFailure } from "./PhaseBadge";

const { wake, stop } = vi.hoisted(() => ({ wake: vi.fn(), stop: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, wake, stop } };
});

beforeEach(() => {
  wake.mockReset();
  stop.mockReset();
});

function notRunning(
  phase: Phase,
  desiredState?: "Running" | "Stopped",
  failure: StartFailure | null = null,
  onWoken = vi.fn(),
) {
  return (
    <NotRunning
      title="Server is asleep"
      body="Wake it to manage players."
      serverName="survival"
      phase={phase}
      desiredState={desiredState}
      failure={failure}
      autoRestarts={1}
      onWoken={onWoken}
    />
  );
}

describe("NotRunning", () => {
  it("offers no wake for a server given up or being deleted, only why", () => {
    render(
      <NotRunning
        title="Server is asleep"
        body="Wake it to manage players."
        serverName="survival"
        phase="Stopped"
        desiredState="Stopped"
        retiring={{ requested_at: new Date().toISOString(), delete: false }}
        onWoken={vi.fn()}
      />,
    );
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("Given up")).toBeTruthy();
  });

  it("offers the wake for a server that is down and meant to stay down", () => {
    render(notRunning("Stopped", "Stopped"));
    expect(screen.getByText("Server is asleep")).toBeTruthy();
    expect(screen.getByText("Wake it to manage players.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Wake" })).toHaveProperty("disabled", false);
  });

  it("says a woken server is starting and offers nothing to press", () => {
    for (const [phase, desired] of [
      ["Stopped", "Running"],
      ["Starting", "Running"],
      ["Starting", undefined],
    ] as const) {
      const { unmount } = render(notRunning(phase, desired));
      expect(screen.getByText("Server is starting"), `${phase}/${desired}`).toBeTruthy();
      expect(screen.getByText(/updates the startup status automatically/), `${phase}/${desired}`).toBeTruthy();
      expect(screen.queryByRole("button"), `${phase}/${desired}`).toBeNull();
      unmount();
    }
  });

  it("says a server on its way down is shutting down", () => {
    for (const [phase, desired] of [
      ["Stopping", undefined],
      ["Stopping", "Stopped"],
      ["Running", "Stopped"],
    ] as const) {
      const { unmount } = render(notRunning(phase, desired));
      expect(screen.getByText("Server is shutting down"), `${phase}/${desired}`).toBeTruthy();
      expect(screen.queryByRole("button"), `${phase}/${desired}`).toBeNull();
      unmount();
    }
  });

  it("offers a failed start its retry and stop", () => {
    render(notRunning("Failed", "Running", "gaveUp"));
    expect(screen.getByText("Server failed to start")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry start" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
  });

  it("counts the retries of a start the operator is still retrying", () => {
    render(notRunning("Failed", "Running", "retrying"));
    expect(screen.getByText("Start timed out — retrying")).toBeTruthy();
    expect(screen.getByText(/1 of 3 retries used/)).toBeTruthy();
  });

  it("turns into the starting notice once the page rereads a wake sent from it", async () => {
    wake.mockResolvedValue(undefined);
    const onWoken = vi.fn();
    const { rerender } = render(notRunning("Stopped", "Stopped", null, onWoken));

    await userEvent.click(screen.getByRole("button", { name: "Wake" }));
    expect(wake).toHaveBeenCalledWith("survival");
    expect(onWoken).toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Waking…" })).toHaveProperty("disabled", true);

    rerender(notRunning("Stopped", "Running", null, onWoken));
    expect(screen.getByRole("status").textContent).toContain("Server is starting");
  });
});

describe("RefreshError", () => {
  it("says the shown status is the last one read, and why the reread failed", () => {
    const err = { status: 429, code: "quota_exceeded", message: "raw" };
    render(<RefreshError error={err} />);
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("Refresh failed. The last successfully retrieved status is displayed.");
    expect(alert.textContent).toContain(humanizeError(err));
  });
});
