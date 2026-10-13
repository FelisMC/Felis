// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DistributedNodes, MigrationDialog } from "./DistributedNodes";
import { RefreshContext, usePageRefresh } from "@/lib/hooks";

const calls = vi.hoisted(() => ({ nodes: vi.fn(), migration: vi.fn(), retryMigration: vi.fn(), migrateServer: vi.fn(), abortMigration: vi.fn() }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});
vi.mock("@/lib/config", async (importActual) => ({
  ...await importActual<typeof import("@/lib/config")>(),
  loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "example.test", build: { version: "v1.4.0", release: "v1.4.0", dev: false } }),
}));

beforeEach(() => {
  vi.clearAllMocks();
  calls.nodes.mockResolvedValue([{ name: "c", role: "worker", ready: true, approved: true }]);
  calls.migration.mockResolvedValue({ id: "persisted-op", state: "failed", sourceNode: "b", targetNode: "c", error: "source node is offline" });
  calls.retryMigration.mockResolvedValue({ id: "persisted-op", state: "backing_up" });
});

describe("durable migration", () => {
  it("loads the persisted operation on opening and retries its ID", async () => {
    const user = userEvent.setup();
    const changed = vi.fn();
    render(<MigrationDialog name="survival" nodeName="b" stopped onChanged={changed} />);
    await user.click(screen.getByRole("button", { name: "Migrate world" }));
    await screen.findByText("source node is offline");
    expect(calls.migration).toHaveBeenCalledWith("survival");
    expect(screen.getByText("persisted-op")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(calls.retryMigration).toHaveBeenCalledWith("survival", "persisted-op"));
    expect(changed).toHaveBeenCalled();
    expect(calls.migrateServer).not.toHaveBeenCalled();
  });

  // A failed migration whose target is gone for good needs a way back to the source node.
  it("abandons a failed migration after confirming, then allows a new one", async () => {
    const user = userEvent.setup();
    const changed = vi.fn();
    calls.abortMigration.mockImplementation(async () => {
      calls.migration.mockResolvedValue({ id: "persisted-op", state: "aborted", sourceNode: "b", targetNode: "c" });
      return { id: "persisted-op", state: "aborted" };
    });
    render(<MigrationDialog name="survival" nodeName="b" stopped onChanged={changed} />);
    await user.click(screen.getByRole("button", { name: "Migrate world" }));
    await screen.findByText("source node is offline");

    await user.click(screen.getByRole("button", { name: "Abandon migration" }));
    expect(calls.abortMigration).not.toHaveBeenCalled();
    expect(screen.getByText(/The unfinished copy on the target node is deleted/)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText(/The unfinished copy on the target node is deleted/)).toBeNull();

    await user.click(screen.getByRole("button", { name: "Abandon migration" }));
    await user.click(screen.getByRole("button", { name: "Abandon migration" }));
    await waitFor(() => expect(calls.abortMigration).toHaveBeenCalledWith("survival", "persisted-op"));
    expect(changed).toHaveBeenCalled();
    expect(await screen.findByText(/Migration abandoned/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Start migration" })).toBeTruthy();
    // A new target can be chosen again.
    expect((screen.getByRole("combobox") as HTMLButtonElement).disabled).toBe(false);
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  });

  it("keeps retry disabled until the server is stopped", async () => {
    const user = userEvent.setup();
    render(<MigrationDialog name="survival" nodeName="b" stopped={false} onChanged={() => {}} />);
    await user.click(screen.getByRole("button", { name: "Migrate world" }));
    const retry = await screen.findByRole("button", { name: "Retry" });
    expect((retry as HTMLButtonElement).disabled).toBe(true);
    await user.click(retry);
    expect(calls.retryMigration).not.toHaveBeenCalled();
  });
});

describe("execution node overview", () => {
  // A controller update leaves workers behind until each is updated from Node operations.
  it("points out workers behind the controller's k3s or felis build", async () => {
    calls.nodes.mockResolvedValue([
      { name: "a", role: "controller", ready: true, approved: true, addresses: [], architecture: "arm64", k3sVersion: "v1.36.4+k3s1" },
      { name: "old-k3s", role: "worker", ready: true, approved: true, addresses: [], architecture: "arm64", k3sVersion: "v1.35.2+k3s1", felisVersion: "v1.4.0" },
      { name: "old-felis", role: "worker", ready: true, approved: true, addresses: [], architecture: "arm64", k3sVersion: "v1.36.4+k3s1", felisVersion: "v1.3.0" },
      { name: "current", role: "worker", ready: true, approved: true, addresses: [], architecture: "arm64", k3sVersion: "v1.36.4+k3s1", felisVersion: "v1.4.0" },
      { name: "unrecorded", role: "worker", ready: true, approved: true, addresses: [], architecture: "arm64", k3sVersion: "v1.36.4+k3s1" },
      { name: "stray", role: "", ready: true, approved: false, addresses: [], architecture: "arm64", k3sVersion: "v1.35.2+k3s1", felisVersion: "v1.3.0" },
    ]);
    render(<DistributedNodes />);
    const row = async (name: string) => (await screen.findByText(name)).closest("li")!;
    await waitFor(async () => expect((await row("old-felis")).textContent).toContain("Update needed"));
    expect((await row("old-k3s")).textContent).toContain("Update needed");
    expect((await row("current")).textContent).not.toContain("Update needed");
    expect((await row("unrecorded")).textContent).not.toContain("Update needed");
    expect((await row("a")).textContent).not.toContain("Update needed");
    // Only enrolled workers are updated from Node operations.
    expect((await row("stray")).textContent).not.toContain("Update needed");
  });

  it("shows a failed read and reloads with the page without interpreting failure as an empty cluster", async () => {
    calls.nodes.mockRejectedValueOnce({ status: 503, code: "distributed_unavailable" });
    calls.nodes.mockResolvedValueOnce([]);
    function Page() {
      const cards = usePageRefresh();
      return <RefreshContext.Provider value={cards.scope}><button type="button" onClick={cards.refresh}>Reload</button><DistributedNodes /></RefreshContext.Provider>;
    }
    render(<Page />);
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "Distributed deployment is not configured. Configure the controller and worker nodes using the deployment runbook first.");
    expect(screen.queryByText(/No execution nodes were found/)).toBeNull();
    expect(screen.getAllByRole("button").map((button) => button.textContent)).toEqual(["Reload"]);
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(await screen.findByText(/No execution nodes were found/)).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
