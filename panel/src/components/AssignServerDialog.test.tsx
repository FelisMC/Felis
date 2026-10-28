// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { ToastProvider } from "@/components/Toaster";
import type { FleetServer, UserView } from "@/lib/types";
import { AssignServerDialog } from "./AssignServerDialog";

const calls = vi.hoisted(() => ({
  previewServerAssignment: vi.fn(),
  assignServer: vi.fn(),
  fleet: vi.fn(),
  listUsers: vi.fn(),
}));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string, options: Record<string, string | number> = {}) => i18next.t(key, options);
const onOpenChange = vi.fn();
const onAssigned = vi.fn();

const SERVER = { name: "survival", label: "Survival SMP", owner: "owner@example.test" };
const ALICE = { id: "u-2", label: "alice", identity: "alice@example.test" };

function renderDialog(props: Partial<Parameters<typeof AssignServerDialog>[0]>) {
  render(
    <ToastProvider>
      <AssignServerDialog open onOpenChange={onOpenChange} onAssigned={onAssigned} {...props} />
    </ToastProvider>,
  );
  return userEvent.setup();
}

const confirm = (dialog: HTMLElement) => within(dialog).getByRole("button", { name: t("admin:assign_confirm") }) as HTMLButtonElement;

function fleetRow(name: string, over: Partial<FleetServer>): FleetServer {
  return { name, subdomain: name, phase: "Stopped", ready: false, playersOnline: 0, playersMax: 20, owned: false, claimable: false, ...over } as FleetServer;
}

function userRow(id: string, username: string, over: Partial<UserView> = {}): UserView {
  return {
    id, username, email: `${username}@example.test`, role: "user", disabled: false, email_verified: true,
    server_count: 0, created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", ...over,
  };
}

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  onOpenChange.mockReset();
  onAssigned.mockReset();
});

describe("AssignServerDialog", () => {
  it("lists what the account would exceed and holds the assignment until that is confirmed", async () => {
    const over = [
      { resource: "servers", limit: 1, after: 2 },
      { resource: "cpu_milli", limit: 1500, after: 2000 },
      { resource: "memory_mb", limit: 4096, after: 6144 },
      { resource: "storage_mb", limit: 10240, after: 20480 },
    ];
    calls.previewServerAssignment.mockResolvedValue({ name: "survival", owner_id: "u-1", over });
    calls.assignServer.mockResolvedValue({ name: "survival", owner_id: "u-2", over });
    const user = renderDialog({ server: SERVER, user: ALICE });
    const dialog = await screen.findByRole("dialog");

    expect(await within(dialog).findByText(t("admin:assign_over_servers", { after: 2, limit: 1 }))).toBeTruthy();
    const cores = (n: number) => t("admin:user_quota_cores", { n });
    const gb = (n: number) => t("admin:user_quota_gb", { n });
    expect(within(dialog).getByText(t("admin:assign_over_cpu_milli", { after: cores(2), limit: cores(1.5) }))).toBeTruthy();
    expect(within(dialog).getByText(t("admin:assign_over_memory_mb", { after: gb(6), limit: gb(4) }))).toBeTruthy();
    expect(within(dialog).getByText(t("admin:assign_over_storage_mb", { after: gb(20), limit: gb(10) }))).toBeTruthy();
    expect(within(dialog).getByText(t("admin:assign_change", { server: "Survival SMP", from: "owner@example.test", to: "alice" }))).toBeTruthy();
    expect(confirm(dialog).disabled).toBe(true);

    await user.click(within(dialog).getByRole("checkbox", { name: t("admin:assign_allow_over") }));
    expect(confirm(dialog).disabled).toBe(false);
    await user.click(confirm(dialog));

    expect(calls.previewServerAssignment).toHaveBeenCalledWith("survival", "u-2");
    expect(calls.assignServer).toHaveBeenCalledExactlyOnceWith("survival", { user_id: "u-2", allow_over_quota: true });
    expect(await screen.findByText(t("admin:assign_done", { server: "Survival SMP", user: "alice" }))).toBeTruthy();
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onAssigned).toHaveBeenCalledTimes(1);
  });

  it("assigns at once, without a confirmation box, when the server fits the quota", async () => {
    calls.previewServerAssignment.mockResolvedValue({ name: "survival", owner_id: "", over: [] });
    calls.assignServer.mockResolvedValue({ name: "survival", owner_id: "u-2", over: [] });
    const user = renderDialog({ server: SERVER, user: ALICE });
    const dialog = await screen.findByRole("dialog");

    await waitFor(() => expect(confirm(dialog).disabled).toBe(false));
    expect(within(dialog).queryByRole("checkbox")).toBeNull();
    expect(within(dialog).getByText(t("admin:assign_from_unclaimed", { server: "Survival SMP", to: "alice" }))).toBeTruthy();
    await user.click(confirm(dialog));

    expect(calls.assignServer).toHaveBeenCalledExactlyOnceWith("survival", { user_id: "u-2", allow_over_quota: false });
  });

  it.each(["preview", "assignment"])("shows an account that already owns the server inside the dialog (%s)", async (stage) => {
    const refused = { status: 409, code: "already_owner", message: "raw" };
    if (stage === "preview") {
      calls.previewServerAssignment.mockRejectedValue(refused);
    } else {
      calls.previewServerAssignment.mockResolvedValue({ name: "survival", owner_id: "u-1", over: [] });
      calls.assignServer.mockRejectedValue(refused);
    }
    const user = renderDialog({ server: SERVER, user: ALICE });
    const dialog = await screen.findByRole("dialog");

    if (stage === "assignment") {
      await waitFor(() => expect(confirm(dialog).disabled).toBe(false));
      await user.click(confirm(dialog));
    }
    expect((await within(dialog).findByRole("alert")).textContent).toBe(t("errors:already_owner"));
    expect(confirm(dialog).disabled).toBe(stage === "preview");
    expect(onAssigned).not.toHaveBeenCalled();
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("offers a server only when it can go to the account, and finds it by name or display name", async () => {
    calls.fleet.mockResolvedValue([
      fleetRow("lobby", { system: true }),
      fleetRow("mine", { owner: "alice@example.test" }),
      fleetRow("leaving", { retiring: { requested_at: "2026-09-27T12:00:00Z", delete: false } }),
      fleetRow("survival", { displayName: "Survival SMP", owner: "owner@example.test" }),
      fleetRow("creative", {}),
    ]);
    const user = renderDialog({ user: ALICE });
    const dialog = await screen.findByRole("dialog");

    expect(await within(dialog).findByRole("button", { name: /Survival SMP/ })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: /creative/ })).toBeTruthy();
    for (const hidden of [/lobby/, /mine/, /leaving/]) {
      expect(within(dialog).queryByRole("button", { name: hidden })).toBeNull();
    }

    await user.type(within(dialog).getByRole("textbox", { name: t("admin:assign_find_server") }), "smp");
    expect(within(dialog).queryByRole("button", { name: /creative/ })).toBeNull();
    calls.previewServerAssignment.mockResolvedValue({ name: "survival", owner_id: "u-1", over: [] });
    await user.click(within(dialog).getByRole("button", { name: /Survival SMP/ }));
    await waitFor(() => expect(calls.previewServerAssignment).toHaveBeenCalledWith("survival", "u-2"));
  });

  it("offers an account only when it is active and not the owner already", async () => {
    calls.listUsers.mockResolvedValue({
      users: [userRow("u-1", "owner"), userRow("u-2", "alice"), userRow("u-3", "bob", { disabled: true })],
      total: 3,
    });
    calls.previewServerAssignment.mockResolvedValue({ name: "survival", owner_id: "u-1", over: [] });
    const user = renderDialog({ server: SERVER });
    const dialog = await screen.findByRole("dialog");

    expect(await within(dialog).findByRole("button", { name: /alice/ })).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: /owner/ })).toBeNull();
    expect(within(dialog).queryByRole("button", { name: /bob/ })).toBeNull();

    await user.click(within(dialog).getByRole("button", { name: /alice/ }));
    await waitFor(() => expect(calls.previewServerAssignment).toHaveBeenCalledWith("survival", "u-2"));
  });
});
