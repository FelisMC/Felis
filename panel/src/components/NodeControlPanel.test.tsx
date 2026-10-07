// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import userEvent from "@testing-library/user-event";
import { NodeControlPanel } from "./NodeControlPanel";
import type { NodeControlTask } from "@/lib/types";
const calls = vi.hoisted(() => ({ nodeTasks: vi.fn(), nodeTask: vi.fn(), startNodeTask: vi.fn(), retryNodeTask: vi.fn() }));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: calls }));
const failed: NodeControlTask = { id: "task-1", request: { action: "approve", name: "worker-01", sshTarget: "worker-01", confirmMaintenance: true }, actor: "owner", state: "failed", stage: "approval", startedAt: "2026-10-06T10:00:00Z", error: "Network isolation probe failed", log: "probe: controller port rejected" };
beforeEach(() => {
  vi.clearAllMocks(); calls.nodeTasks.mockResolvedValue({ available: true, tasks: [] }); calls.nodeTask.mockResolvedValue(failed);
});
describe("host node management", () => {
  it("keeps enrollment form visible and disabled while availability is unknown", async () => {
    let resolve!: (value: { available: boolean; tasks: [] }) => void;
    calls.nodeTasks.mockReturnValue(new Promise((r) => { resolve = r; }));
    render(<NodeControlPanel distributed />);
    expect(screen.getByLabelText("Node name")).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: "Execute operation" })).toHaveProperty("disabled", true);
    await act(async () => resolve({ available: true, tasks: [] }));
    expect(screen.getByLabelText("Node name")).toHaveProperty("disabled", false);
  });
  it("updates the default action after the deployment mode loads", async () => {
    const view = render(<NodeControlPanel distributed={null} />);
    await waitFor(() => expect(calls.nodeTasks).toHaveBeenCalled());
    expect(screen.queryByLabelText("Node name")).toBeNull();
    view.rerender(<NodeControlPanel distributed />);
    await waitFor(() => expect(screen.getByLabelText("Node name")).toHaveProperty("disabled", false));
    expect(screen.getByRole("combobox", { name: "Operation" }).textContent).toContain("Enroll and approve worker");
  });
  it("requires maintenance acknowledgement and submits structured enrollment without credentials", async () => {
    const running = { ...failed, state: "running", request: { ...failed.request, action: "join" } };
    calls.startNodeTask.mockResolvedValue(running); calls.nodeTask.mockResolvedValue(running);
    render(<NodeControlPanel distributed />);
    await waitFor(() => expect(screen.getByLabelText("Node name")).toHaveProperty("disabled", false));
    fireEvent.change(screen.getByLabelText("Node name"), { target: { value: "worker-01" } });
    fireEvent.change(screen.getByLabelText("SSH target"), { target: { value: "root@192.0.2.10" } });
    fireEvent.change(screen.getByLabelText("Fixed node IP"), { target: { value: "192.0.2.10" } });
    expect(screen.getByRole("button", { name: "Execute operation" })).toHaveProperty("disabled", true);
    await userEvent.click(screen.getByRole("switch"));
    await userEvent.click(screen.getByRole("button", { name: "Execute operation" }));
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "join", name: "worker-01", sshTarget: "root@192.0.2.10", externalIP: "192.0.2.10", peers: [], confirmMaintenance: true });
    await screen.findByRole("region", { name: "Task progress" });
    expect(screen.getByRole("button", { name: "Execute operation" })).toHaveProperty("disabled", true);
  });
  it("restores failed tasks with concrete errors and logs and retries as a new attempt", async () => {
    calls.nodeTasks.mockResolvedValue({ available: true, tasks: [failed] });
    calls.retryNodeTask.mockResolvedValue({ ...failed, id: "task-2", state: "running" });
    render(<NodeControlPanel distributed />);
    expect(await screen.findByText("Network isolation probe failed")).toBeTruthy();
    expect(screen.getByLabelText("Execution log").textContent).toContain("probe: controller port rejected");
    await userEvent.click(screen.getByRole("button", { name: "Retry operation" }));
    expect(calls.retryNodeTask).toHaveBeenCalledWith("task-1");
    await waitFor(() => expect(calls.nodeTask).toHaveBeenCalledWith("task-2"));
  });
  it("gates worker operations behind explicit activation and explains its required input", async () => {
    const running = { ...failed, state: "running" as const, request: { action: "enable" as const, confirmMaintenance: true } };
    calls.startNodeTask.mockResolvedValue(running); calls.nodeTask.mockResolvedValue(running);
    render(<NodeControlPanel distributed={false} />);
    expect(screen.queryByRole("combobox", { name: "Operation" })).toBeNull();
    expect(screen.queryByLabelText("Fixed node IP")).toBeNull();
    const enable = screen.getByRole("button", { name: "Enable distributed deployment" });
    await waitFor(() => expect(enable).toHaveProperty("disabled", false));
    await userEvent.click(enable);
    expect(screen.getByText(/Required. Enter the controller/)).toBeTruthy();
    expect(screen.queryByLabelText("Node name")).toBeNull();
    await userEvent.click(screen.getByRole("switch"));
    expect(screen.getByRole("button", { name: "Enable distributed deployment" })).toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("Fixed node IP"), { target: { value: "192.0.2.10" } });
    await userEvent.click(screen.getByRole("button", { name: "Enable distributed deployment" }));
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "enable", externalIP: "192.0.2.10", peers: [], confirmMaintenance: true });
  });
  it("reports a host connection failure and disables changes", async () => {
    calls.nodeTasks.mockRejectedValue({ status: 503, code: "node_control_unavailable" });
    render(<NodeControlPanel distributed={false} />);
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Enable distributed deployment" })).toHaveProperty("disabled", true);
    expect(screen.queryByLabelText("Fixed node IP")).toBeNull();
    expect(calls.startNodeTask).not.toHaveBeenCalled();
  });
});
