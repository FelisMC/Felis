// @vitest-environment jsdom
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DistributedNodes, MigrationDialog } from "./DistributedNodes";
import type { FailoverPlan } from "@/lib/types";

const calls = vi.hoisted(() => ({ nodes: vi.fn(), failoverPlan: vi.fn(), failOverNode: vi.fn(), migration: vi.fn(), startNodeTask: vi.fn() }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});
const tier = vi.hoisted(() => ({ isOwner: true }));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
const config = vi.hoisted(() => ({ controllerHA: false }));
vi.mock("@/lib/config", async (importActual) => ({
  ...await importActual<typeof import("@/lib/config")>(),
  loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "example.test", distributed: true, controllerHA: config.controllerHA }),
}));

// Radix Select opens through pointer capture, which jsdom lacks.
beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000).toISOString();

const plan = (): FailoverPlan => ({
  node: "worker-b",
  lost: true,
  fenced: false,
  lostSince: minutesAgo(25),
  targets: ["a", "worker-c"],
  servers: [
    { name: "alice", desiredState: "Running", backup: { id: "bk-1", createdAt: minutesAgo(40), sizeBytes: 3 * 1024 * 1024 }, operation: null },
    { name: "bob", desiredState: "Stopped", backup: null, operation: null },
  ],
});

beforeEach(() => {
  vi.clearAllMocks();
  tier.isOwner = true;
  config.controllerHA = false;
  calls.nodes.mockResolvedValue([
    { name: "a", role: "controller", ready: true, approved: false, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
    { name: "worker-b", role: "worker", ready: false, approved: true, fenced: false, lostSince: minutesAgo(25), addresses: [], architecture: "arm64", k3sVersion: "v1" },
    { name: "worker-c", role: "worker", ready: true, approved: true, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
    { name: "worker-d", role: "worker", ready: false, approved: false, fenced: true, lostSince: minutesAgo(300), addresses: [], architecture: "arm64", k3sVersion: "v1" },
    { name: "stray", role: "", ready: false, approved: false, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
  ]);
  calls.failoverPlan.mockResolvedValue(plan());
});

const row = async (name: string) => (await screen.findByText(name)).closest("li")!;

describe("failover of a lost worker", () => {
  it("offers a failover only to the Owner, only on lost workers, and marks fenced and lost nodes", async () => {
    render(<DistributedNodes />);
    const lost = await row("worker-b");
    expect(within(lost).getByRole("button", { name: "Fail over" })).toBeTruthy();
    expect(lost.textContent).toContain("Lost 25 minutes ago");
    expect(within(await row("worker-c")).queryByRole("button", { name: "Fail over" })).toBeNull();
    expect(within(await row("a")).queryByRole("button", { name: "Fail over" })).toBeNull();
    // A node without the worker role holds no worlds to fail over.
    expect(within(await row("stray")).queryByRole("button", { name: "Fail over" })).toBeNull();
    expect((await row("worker-d")).textContent).toContain("Fenced");
  });

  // With controller high availability a lost controller's worlds fail over too, and every
  // controller among the targets is marked as one.
  it("offers a failover on a lost controller only with controller high availability", async () => {
    const user = userEvent.setup();
    calls.nodes.mockResolvedValue([
      { name: "a", role: "controller", ready: false, approved: false, fenced: false, lostSince: minutesAgo(5), addresses: [], architecture: "arm64", k3sVersion: "v1" },
      { name: "a2", role: "controller", ready: true, approved: false, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
      { name: "a3", role: "controller", ready: true, approved: false, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
      { name: "worker-c", role: "worker", ready: true, approved: true, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
    ]);
    calls.failoverPlan.mockResolvedValue({ ...plan(), node: "a", targets: ["a3", "worker-c"] });
    const { unmount } = render(<DistributedNodes />);
    expect((await row("a")).textContent).toContain("Lost 5 minutes ago");
    expect(within(await row("a")).queryByRole("button", { name: "Fail over" })).toBeNull();
    unmount();
    config.controllerHA = true;
    render(<DistributedNodes />);
    await user.click(await within(await row("a")).findByRole("button", { name: "Fail over" }));
    expect(within(await row("a2")).queryByRole("button", { name: "Fail over" })).toBeNull();
    await waitFor(() => expect(calls.failoverPlan).toHaveBeenCalledWith("a"));
    await user.click(await screen.findByRole("combobox"));
    expect(await screen.findByRole("option", { name: "a3 (controller)" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "worker-c" })).toBeTruthy();
  });

  // A fenced controller back online returns to service from its row, after a confirmation;
  // a fenced worker goes through approval instead, and an offline one waits.
  it("returns a fenced controller that is back online to service", async () => {
    const user = userEvent.setup();
    config.controllerHA = true;
    calls.nodes.mockResolvedValue([
      { name: "a", role: "controller", ready: true, approved: false, fenced: true, addresses: [], architecture: "arm64", k3sVersion: "v1" },
      { name: "a2", role: "controller", ready: false, approved: false, fenced: true, lostSince: minutesAgo(5), addresses: [], architecture: "arm64", k3sVersion: "v1" },
      { name: "a3", role: "controller", ready: true, approved: false, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
      { name: "worker-b", role: "worker", ready: true, approved: false, fenced: true, addresses: [], architecture: "arm64", k3sVersion: "v1" },
    ]);
    calls.startNodeTask.mockResolvedValue({ id: "t1", request: { action: "unfence", name: "a", confirmMaintenance: true }, actor: "owner", state: "running", stage: "unfence", startedAt: minutesAgo(0) });
    render(<DistributedNodes />);
    for (const name of ["a2", "a3", "worker-b"]) expect(within(await row(name)).queryByRole("button", { name: "Return to service" })).toBeNull();
    await user.click(within(await row("a")).getByRole("button", { name: "Return to service" }));
    expect(calls.startNodeTask).not.toHaveBeenCalled();
    expect(screen.getByText(/servers moved away by the failover stay on their new nodes/)).toBeTruthy();
    await user.click(within(await row("a")).getByRole("button", { name: "Return to service" }));
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "unfence", name: "a", confirmMaintenance: true }));
    expect(await screen.findByText(/Follow it in the task history/)).toBeTruthy();
  });

  it("leaves fenced controllers out of the migration targets", async () => {
    calls.nodes.mockResolvedValue([
      { name: "a", role: "controller", ready: true, approved: false, fenced: true, addresses: [], architecture: "arm64", k3sVersion: "v1" },
      { name: "a3", role: "controller", ready: true, approved: false, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
      { name: "worker-c", role: "worker", ready: true, approved: true, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
    ]);
    calls.migration.mockRejectedValue({ status: 404 });
    const user = userEvent.setup();
    render(<MigrationDialog name="survival" nodeName="worker-c" stopped onChanged={() => {}} />);
    await user.click(screen.getByRole("button", { name: "Migrate world" }));
    await user.click(await screen.findByRole("combobox"));
    expect(await screen.findByRole("option", { name: "a3 (controller)" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "a (controller)" })).toBeNull();
  });

  it("hides the failover from an administrator who is not the Owner", async () => {
    tier.isOwner = false;
    render(<DistributedNodes />);
    await row("worker-b");
    expect(screen.queryByRole("button", { name: "Fail over" })).toBeNull();
  });

  it("shows what each server loses and needs the target and the typed node name before starting", async () => {
    const user = userEvent.setup();
    calls.failOverNode.mockResolvedValue({
      operations: [{ id: "fo-alice", kind: "failover", server: "alice", state: "fencing", stage: "fencing", sourceNode: "worker-b", targetNode: "worker-c", sourcePVC: "", targetPVC: "", switched: false, attempt: 0 }],
      skipped: [{ name: "bob", reason: "no_backup" }],
      fenceError: "a node operation is already running",
    });
    render(<DistributedNodes />);
    await user.click(within(await row("worker-b")).getByRole("button", { name: "Fail over" }));
    const dialog = await screen.findByRole("dialog");
    await within(dialog).findByText(/Restores the backup from 40 minutes ago \(3\.0 MiB\); later changes are lost/);
    expect(within(dialog).getByText(/No intact backup is available/)).toBeTruthy();
    expect(within(dialog).getByText("Starts after the failover")).toBeTruthy();
    expect(within(dialog).getByText("Stays stopped after the failover")).toBeTruthy();

    const start = within(dialog).getByRole("button", { name: "Start failover" }) as HTMLButtonElement;
    expect(start.disabled).toBe(true);
    const confirm = within(dialog).getByLabelText("Type the node name worker-b to confirm");
    await user.type(confirm, "worker-");
    expect(start.disabled).toBe(true);
    await user.type(confirm, "b");
    // The name alone is not enough: a target must be chosen too.
    expect(start.disabled).toBe(true);
    await user.click(within(dialog).getByRole("combobox"));
    expect(await screen.findByRole("option", { name: "a (controller)" })).toBeTruthy();
    await user.click(screen.getByRole("option", { name: "worker-c" }));
    expect(start.disabled).toBe(false);

    calls.failoverPlan.mockResolvedValue({ ...plan(), servers: [{ ...plan().servers[0], operation: { id: "fo-alice", kind: "failover", server: "alice", state: "restoring", stage: "restoring", sourceNode: "worker-b", targetNode: "worker-c", sourcePVC: "", targetPVC: "", switched: false, attempt: 0, resume: "Running" } }, plan().servers[1]] });
    await user.click(start);
    await waitFor(() => expect(calls.failOverNode).toHaveBeenCalledWith("worker-b", "worker-c", "worker-b"));
    expect(await within(dialog).findByText("Failover started for 1 server(s).")).toBeTruthy();
    expect(within(dialog).getByText("bob: no intact backup, stays on the lost node")).toBeTruthy();
    expect(within(dialog).getByText(/The node could not be fenced yet \(a node operation is already running\)/)).toBeTruthy();
    expect(await within(dialog).findByText("Restoring and verifying the backup on worker-c")).toBeTruthy();
    // Nothing is left to start: the only server with a backup is already failing over.
    expect(within(dialog).queryByRole("button", { name: "Start failover" })).toBeNull();
  });

  it("refuses an online node and starts nothing", async () => {
    const user = userEvent.setup();
    calls.failoverPlan.mockResolvedValue({ ...plan(), lost: false });
    render(<DistributedNodes />);
    await user.click(within(await row("worker-b")).getByRole("button", { name: "Fail over" }));
    const dialog = await screen.findByRole("dialog");
    expect(await within(dialog).findByText(/This node is online/)).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: "Start failover" })).toBeNull();
    expect(calls.failOverNode).not.toHaveBeenCalled();
  });
});

describe("migration targets", () => {
  it("offers the controller as a target for a world on a worker, and not for one on the controller", async () => {
    const user = userEvent.setup();
    calls.migration.mockRejectedValue({ status: 404 });
    calls.nodes.mockResolvedValue([
      { name: "a", role: "controller", ready: true, approved: false, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
      { name: "worker-c", role: "worker", ready: true, approved: true, fenced: false, addresses: [], architecture: "arm64", k3sVersion: "v1" },
    ]);
    const { unmount } = render(<MigrationDialog name="survival" nodeName="worker-c" stopped onChanged={() => {}} />);
    await user.click(screen.getByRole("button", { name: "Migrate world" }));
    await user.click(await screen.findByRole("combobox"));
    expect(await screen.findByRole("option", { name: "a (controller)" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "worker-c" })).toBeNull();
    unmount();

    render(<MigrationDialog name="survival" stopped onChanged={() => {}} />);
    await user.click(screen.getByRole("button", { name: "Migrate world" }));
    await user.click(await screen.findByRole("combobox"));
    expect(await screen.findByRole("option", { name: "worker-c" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "a (controller)" })).toBeNull();
  });
});
