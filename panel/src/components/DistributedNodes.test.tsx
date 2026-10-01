// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MigrationDialog } from "./DistributedNodes";

const calls = vi.hoisted(() => ({ nodes: vi.fn(), migration: vi.fn(), retryMigration: vi.fn(), migrateServer: vi.fn() }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

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
    await user.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(calls.retryMigration).toHaveBeenCalledWith("survival", "persisted-op"));
    expect(changed).toHaveBeenCalled();
    expect(calls.migrateServer).not.toHaveBeenCalled();
  });

  it("keeps retry disabled until the server is stopped", async () => {
    const user = userEvent.setup();
    render(<MigrationDialog name="survival" nodeName="b" stopped={false} onChanged={() => {}} />);
    await user.click(screen.getByRole("button", { name: "Migrate world" }));
    const retry = await screen.findByRole("button", { name: "Try again" });
    expect((retry as HTMLButtonElement).disabled).toBe(true);
    await user.click(retry);
    expect(calls.retryMigration).not.toHaveBeenCalled();
  });
});
