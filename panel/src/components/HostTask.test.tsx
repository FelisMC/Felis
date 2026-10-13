// @vitest-environment jsdom
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { HostTaskNotice, HostTaskStatus, useHostTask } from "./HostTask";
import type { NodeControlTask } from "@/lib/types";

const calls = vi.hoisted(() => ({ nodeTasks: vi.fn(), nodeTask: vi.fn(), startNodeTask: vi.fn(), nodeTaskArtifact: vi.fn() }));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: calls }));
const saved = vi.hoisted(() => vi.fn());
vi.mock("@/lib/download", () => ({ saveBlob: saved }));

const task = (over: Partial<NodeControlTask>): NodeControlTask => ({
  id: "t1", request: { action: "doctor", confirmMaintenance: false }, actor: "owner", state: "running", stage: "preflight", startedAt: "2026-10-12T00:00:00Z", ...over,
});

function Harness() {
  const host = useHostTask(["doctor"]);
  return <div>
    <HostTaskNotice otherRunning={host.otherRunning} available={host.available} loading={host.loading} />
    <button type="button" disabled={host.busy} onClick={() => void host.start({ action: "doctor", confirmMaintenance: false })}>run</button>
    {host.task && <HostTaskStatus task={host.task} />}
    {host.error && <p>{host.error}</p>}
  </div>;
}

beforeEach(() => {
  vi.clearAllMocks();
  calls.nodeTasks.mockResolvedValue({ available: true, tasks: [] });
});

describe("host tasks", () => {
  it("starts a task and follows it, holding further starts while it runs", async () => {
    const running = task({ id: "t9" });
    calls.startNodeTask.mockResolvedValue(running);
    calls.nodeTask.mockResolvedValue(running);
    render(<Harness />);
    await waitFor(() => expect(screen.getByRole("button", { name: "run" })).toHaveProperty("disabled", false));
    await userEvent.click(screen.getByRole("button", { name: "run" }));
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "doctor", confirmMaintenance: false });
    await screen.findByRole("region", { name: "Task progress" });
    expect(calls.nodeTask).toHaveBeenCalledWith("t9");
    expect(screen.getByRole("button", { name: "run" })).toHaveProperty("disabled", true);
  });

  it("follows the newest task of its own actions and waits while another runs", async () => {
    calls.nodeTasks.mockResolvedValue({ available: true, tasks: [
      task({ id: "other", request: { action: "db-restore", confirmMaintenance: true } }),
      task({ id: "mine", state: "succeeded", stage: "complete" }),
    ] });
    calls.nodeTask.mockImplementation(async (id: string) => task({ id, state: "succeeded", stage: "complete" }));
    render(<Harness />);
    expect(await screen.findByText("Another host task is running. This action becomes available when it finishes.")).toBeTruthy();
    await waitFor(() => expect(calls.nodeTask).toHaveBeenCalledWith("mine"));
    expect(screen.getByRole("button", { name: "run" })).toHaveProperty("disabled", true);
  });

  it("says when the host service is not installed", async () => {
    calls.nodeTasks.mockResolvedValue({ available: false, tasks: [] });
    render(<Harness />);
    expect(await screen.findByText(/Host node service is not installed/)).toBeTruthy();
  });

  it("links the Cloudflare page the host waits on, and only while it waits", () => {
    const url = "https://dash.cloudflare.com/argotunnel?aud=&callback=https%3A%2F%2Flogin.cloudflareaccess.org%2Fx";
    const { rerender } = render(<HostTaskStatus task={task({ request: { action: "connection-configure", confirmMaintenance: true }, stage: "cloudflare_login", log: `Please open the following URL:\n${url}\n` })} />);
    expect(screen.getByRole("link", { name: "Open Cloudflare authorization" }).getAttribute("href")).toBe(url);
    expect(screen.getByText("Awaiting Cloudflare authorization")).toBeTruthy();
    rerender(<HostTaskStatus task={task({ state: "succeeded", stage: "complete", log: url })} />);
    expect(screen.queryByRole("link", { name: "Open Cloudflare authorization" })).toBeNull();
  });

  it("downloads a finished task's file", async () => {
    const blob = new Blob(["bundle"]);
    calls.nodeTaskArtifact.mockResolvedValue({ blob, filename: "felis-support-1.tar.gz" });
    render(<HostTaskStatus task={task({ state: "succeeded", stage: "complete", artifact: "/var/lib/felis/support/felis-support-1.tar.gz" })} />);
    await userEvent.click(screen.getByRole("button", { name: "Download" }));
    await waitFor(() => expect(saved).toHaveBeenCalledWith(blob, "felis-support-1.tar.gz"));
    expect(calls.nodeTaskArtifact).toHaveBeenCalledWith("t1");
  });

  it("offers a retry for a failed task", async () => {
    const retry = vi.fn();
    render(<HostTaskStatus task={task({ state: "failed", error: "felis offsite failed" })} onRetry={retry} />);
    expect(screen.getByText("felis offsite failed")).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Retry operation" }));
    expect(retry).toHaveBeenCalled();
  });
});
