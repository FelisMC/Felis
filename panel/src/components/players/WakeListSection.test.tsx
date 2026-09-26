// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WakeListSection } from "./WakeListSection";
import type { AllowlistEntry } from "@/lib/types";

const calls = vi.hoisted(() => ({ serverAllowlist: vi.fn(), setAllowlistWake: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const STEVE = "0f8fad5b-d9cb-469f-a165-70867728950e";
const GHOST = "7c9e6679-7425-40de-944b-e07fc1f90ae7";
const ALEX = "16fd2706-8baf-433b-82eb-8c7fada847da";
const joined = new Date(Date.now() - 3 * 86400_000).toISOString();
const entry = (mc_uuid: string, can_wake: boolean, username?: string): AllowlistEntry => ({
  mc_uuid,
  username,
  added_at: joined,
  can_wake,
});

beforeEach(() => {
  calls.serverAllowlist.mockReset();
  calls.setAllowlistWake.mockReset();
});

function rowOf(label: string) {
  return screen.getByText(label).closest("li") as HTMLElement;
}

describe("WakeListSection", () => {
  it("lists who may wake the server, marks revoked players and names unlinked ones", async () => {
    calls.serverAllowlist.mockResolvedValue([entry(STEVE, true, "Steve"), entry(GHOST, false)]);
    render(<WakeListSection name="lobby" policy="allowlist" defaultOpen />);

    const steve = await screen.findByText("Steve");
    expect(calls.serverAllowlist).toHaveBeenCalledWith("lobby");
    const steveRow = steve.closest("li") as HTMLElement;
    expect(within(steveRow).queryByText("Revoked")).toBeNull();
    expect(within(steveRow).getByText("0f8fad5b").getAttribute("title")).toBe(STEVE);
    expect(within(steveRow).getByText("first joined 3 days ago")).toBeTruthy();
    expect(within(steveRow).getByRole("button", { name: "Take away Steve's right to wake the server" }).textContent).toBe(
      "Revoke",
    );

    const ghostRow = rowOf("Player without a linked account");
    expect(within(ghostRow).getByText("Revoked")).toBeTruthy();
    expect(
      within(ghostRow).getByRole("button", { name: "Allow Player without a linked account to wake the server again" })
        .textContent,
    ).toBe("Allow again");
    // The list is in effect: no note about the policy.
    expect(screen.queryByText(/this list has no effect/)).toBeNull();
  });

  it("says the list has no effect under the other two policies", async () => {
    calls.serverAllowlist.mockResolvedValue([]);
    const owner = render(<WakeListSection name="lobby" policy="ownerOnly" defaultOpen />);
    expect(await screen.findByText(/Owner only, so only you and admins/)).toBeTruthy();
    expect(screen.getByText("No player has joined this server yet.")).toBeTruthy();
    owner.unmount();

    render(<WakeListSection name="lobby" policy="public" defaultOpen />);
    expect(await screen.findByText(/Public, so any player's join starts it/)).toBeTruthy();
  });

  it("revokes and restores in place, reading the list again after each change", async () => {
    const user = userEvent.setup();
    calls.serverAllowlist.mockResolvedValue([entry(STEVE, true, "Steve"), entry(GHOST, false)]);
    calls.setAllowlistWake.mockResolvedValue(undefined);
    render(<WakeListSection name="lobby" policy="allowlist" defaultOpen />);
    await screen.findByText("Steve");

    calls.serverAllowlist.mockResolvedValue([entry(STEVE, false, "Steve"), entry(GHOST, false)]);
    await user.click(screen.getByRole("button", { name: "Take away Steve's right to wake the server" }));
    expect(calls.setAllowlistWake).toHaveBeenCalledWith("lobby", STEVE, false);
    expect((await screen.findByRole("status")).textContent).toBe("Steve can no longer wake the server.");
    expect(await within(rowOf("Steve")).findByText("Revoked")).toBeTruthy();
    expect(calls.serverAllowlist).toHaveBeenCalledTimes(2);

    calls.serverAllowlist.mockResolvedValue([entry(STEVE, false, "Steve"), entry(GHOST, true)]);
    await user.click(
      screen.getByRole("button", { name: "Allow Player without a linked account to wake the server again" }),
    );
    expect(calls.setAllowlistWake).toHaveBeenLastCalledWith("lobby", GHOST, true);
    expect((await screen.findByRole("status")).textContent).toBe(
      "Player without a linked account can wake the server again.",
    );
  });

  it("reports a refused change and a failed read", async () => {
    const user = userEvent.setup();
    calls.serverAllowlist.mockResolvedValue([entry(STEVE, true, "Steve")]);
    calls.setAllowlistWake.mockRejectedValue({ status: 403, code: "forbidden", message: "forbidden" });
    const first = render(<WakeListSection name="lobby" policy="allowlist" defaultOpen />);
    await user.click(await screen.findByRole("button", { name: "Take away Steve's right to wake the server" }));
    expect((await screen.findByRole("alert")).textContent).toBe("You are not allowed to do that.");
    expect(within(rowOf("Steve")).queryByText("Revoked")).toBeNull();
    first.unmount();

    calls.serverAllowlist.mockRejectedValue({ status: 500, code: "internal", message: "db down" });
    render(<WakeListSection name="lobby" policy="allowlist" defaultOpen />);
    expect((await screen.findByRole("alert")).textContent).toBe("Couldn't load the wake list.");
  });

  it("finds a player by account name or by UUID once the list is long", async () => {
    const user = userEvent.setup();
    const many = Array.from({ length: 9 }, (_, i) =>
      entry(`00000000-0000-0000-0000-00000000000${i}`, true, `Player${i}`),
    );
    calls.serverAllowlist.mockResolvedValue([...many, entry(ALEX, true, "Alex")]);
    render(<WakeListSection name="lobby" policy="allowlist" defaultOpen />);
    await screen.findByText("Player0");
    // Ten a page: Alex, tenth, is on the first page until a search narrows it.
    const search = screen.getByPlaceholderText("Search players…");

    await user.type(search, "alex");
    expect(screen.getByText("Alex")).toBeTruthy();
    expect(screen.queryByText("Player0")).toBeNull();

    await user.clear(search);
    await user.type(search, "8baf-433b");
    expect(screen.getByText("Alex")).toBeTruthy();
    expect(screen.queryByText("Player1")).toBeNull();
  });
});

describe("WakeListSection while a change is in flight", () => {
  it("holds every row's button until the change lands, so two cannot cross", async () => {
    const user = userEvent.setup();
    calls.serverAllowlist.mockResolvedValue([entry(STEVE, true, "Steve"), entry(GHOST, false)]);
    let land: () => void = () => {};
    calls.setAllowlistWake.mockReturnValue(new Promise<void>((resolve) => (land = resolve)));
    render(<WakeListSection name="lobby" policy="allowlist" defaultOpen />);
    await user.click(await screen.findByRole("button", { name: "Take away Steve's right to wake the server" }));

    const other = within(rowOf("Player without a linked account")).getByRole("button");
    expect((other as HTMLButtonElement).disabled).toBe(true);
    land();
    await screen.findByRole("status");
    expect((other as HTMLButtonElement).disabled).toBe(false);
  });
});
