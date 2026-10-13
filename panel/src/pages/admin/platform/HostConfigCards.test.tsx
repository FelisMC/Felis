// @vitest-environment jsdom
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AccessCard, ConvergeCard, CredentialsCard, ProxyCard, StorageCard } from "./HostConfigCards";
import { ForwardingDialog } from "@/components/ForwardingDialog";
import type { AsyncState } from "@/lib/hooks";
import type { HostOverview, NodeControlTask } from "@/lib/types";

const calls = vi.hoisted(() => ({ nodeTasks: vi.fn(), nodeTask: vi.fn(), startNodeTask: vi.fn() }));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: calls }));

const overview: HostOverview = {
  host: "ctl-a", at: "2026-10-12T00:00:00Z", version: "v1", disks: [{ path: "/var", total: 100, available: 5 }], units: [], proxy: {},
  alerts: [], databaseBackups: [], offsite: { configured: false, keyKept: false },
  settings: { dbBackupTime: "03:30", dbBackupKeep: 14, velocityXmx: "1G" },
  connection: { mode: "local", panelHost: "console.example.com", adminHost: "op.console.example.com" },
  storage: { mode: "local" }, rootDomain: "example.com",
};
const state = (data: HostOverview): AsyncState<HostOverview> => ({ data, error: null, loading: false, updatedAt: 0, reload: vi.fn() });
const started = (r: NodeControlTask["request"]): NodeControlTask => ({ id: "t1", request: r, actor: "owner", state: "running", stage: "preflight", startedAt: "2026-10-12T00:00:00Z" });

beforeEach(() => {
  vi.clearAllMocks();
  calls.nodeTasks.mockResolvedValue({ available: true, tasks: [] });
  calls.startNodeTask.mockImplementation(async (r) => started(r));
  calls.nodeTask.mockImplementation(async () => started({ action: "doctor", confirmMaintenance: false }));
});

function finishInstantly() {
  calls.startNodeTask.mockImplementation(async (r) => ({ ...started(r), state: "succeeded", stage: "complete" }));
  calls.nodeTask.mockImplementation(async () => ({ ...started({ action: "doctor", confirmMaintenance: false }), state: "succeeded", stage: "complete" }));
}

async function enabled(name: string) {
  await waitFor(() => expect(screen.getByRole("button", { name })).toHaveProperty("disabled", false));
  return screen.getByRole("button", { name });
}
async function confirmDialog(name: string) {
  const dialog = await screen.findByRole("dialog");
  await userEvent.click(within(dialog).getByRole("button", { name }));
}

describe("access and domain", () => {
  it("builds a Cloudflare edge only with an account, an identity and a token, which goes as a secret", async () => {
    render(<AccessCard overview={state(overview)} />);
    await userEvent.click(screen.getByRole("radio", { name: "Cloudflare Tunnel" }));
    const apply = screen.getByRole("button", { name: "Apply access method" });
    fireEvent.change(screen.getByLabelText("Who may sign in"), { target: { value: "@example.com" } });
    fireEvent.change(screen.getByLabelText("Cloudflare account ID"), { target: { value: "a".repeat(32) } });
    expect(apply).toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("Cloudflare API token"), { target: { value: "cf-token" } });
    await userEvent.click(await enabled("Apply access method"));
    await confirmDialog("Apply access method");
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenCalledWith({
      action: "connection-configure",
      params: { mode: "cloudflare", panelHost: "console.example.com", adminHost: "op.console.example.com", identity: "@example.com", accountID: "a".repeat(32) },
      secrets: { apiToken: "cf-token" }, confirmMaintenance: true,
    }));
  });

  it("moves to a reverse proxy and changes the domain after confirmation", async () => {
    finishInstantly();
    render(<AccessCard overview={state(overview)} />);
    expect(screen.getByRole("button", { name: "Apply access method" })).toHaveProperty("disabled", true);
    await userEvent.click(screen.getByRole("radio", { name: "Own reverse proxy" }));
    await userEvent.click(await enabled("Apply access method"));
    await confirmDialog("Apply access method");
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "connection-configure", params: { mode: "reverse-proxy", panelHost: "console.example.com", adminHost: "op.console.example.com" }, secrets: undefined, confirmMaintenance: true }));
    fireEvent.change(screen.getByLabelText("New root domain"), { target: { value: "example.com" } });
    expect(screen.getByRole("button", { name: "Change domain" })).toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("New root domain"), { target: { value: "play.example.org" } });
    await userEvent.click(await enabled("Change domain"));
    await confirmDialog("Change domain");
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenLastCalledWith({ action: "domain-set", params: { domain: "play.example.org" }, confirmMaintenance: true }));
  });
});

describe("upload storage", () => {
  it("moves uploads to a bucket with its keys as secrets", async () => {
    const o = state(overview);
    render(<StorageCard overview={o} />);
    await userEvent.click(screen.getByRole("radio", { name: "S3-compatible bucket" }));
    fireEvent.change(screen.getByLabelText("Endpoint"), { target: { value: "https://s3.example.com" } });
    fireEvent.change(screen.getByLabelText("Bucket"), { target: { value: "uploads" } });
    fireEvent.change(screen.getByLabelText("Access key ID"), { target: { value: "AK" } });
    expect(screen.getByRole("button", { name: "Apply storage" })).toHaveProperty("disabled", true);
    fireEvent.change(screen.getByLabelText("Secret access key"), { target: { value: "SK" } });
    await userEvent.click(await enabled("Apply storage"));
    await confirmDialog("Apply storage");
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "storage-configure", params: { mode: "s3", endpoint: "https://s3.example.com", bucket: "uploads" }, secrets: { accessKey: "AK", secretKey: "SK" }, confirmMaintenance: true }));
    await waitFor(() => expect(o.reload).toHaveBeenCalled());
  });
});

describe("credentials, proxy and completion", () => {
  it("rotates the chosen credential after naming what it interrupts", async () => {
    render(<CredentialsCard />);
    const row = screen.getByText("Forwarding secret").closest("li")!;
    await waitFor(() => expect(within(row).getByRole("button", { name: "Rotate" })).toHaveProperty("disabled", false));
    await userEvent.click(within(row).getByRole("button", { name: "Rotate" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/all players are disconnected/)).toBeTruthy();
    await userEvent.click(within(dialog).getByRole("button", { name: "Rotate" }));
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "rotate-token", params: { kind: "forwarding" }, confirmMaintenance: true }));
  });

  it("takes a proxy heap between 256M and 64G", async () => {
    render(<ProxyCard overview={state(overview)} />);
    const input = screen.getByLabelText("Heap ceiling");
    for (const bad of ["128M", "65G", "1T", "1G"]) {
      fireEvent.change(input, { target: { value: bad } });
      expect(screen.getByRole("button", { name: "Apply" })).toHaveProperty("disabled", true);
    }
    fireEvent.change(input, { target: { value: "2g" } });
    await userEvent.click(await enabled("Apply"));
    await confirmDialog("Apply");
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "host-settings", params: { velocityXmx: "2G" }, confirmMaintenance: true }));
  });

  it("fills in older servers, asking first when it turns RCON on", async () => {
    finishInstantly();
    render(<ConvergeCard />);
    await userEvent.click(await enabled("Run completion"));
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "converge", params: { userRcon: "false" }, confirmMaintenance: false });
    await userEvent.click(screen.getByRole("switch", { name: "Enable RCON on older user servers" }));
    await userEvent.click(await enabled("Run completion"));
    await confirmDialog("Run completion");
    await waitFor(() => expect(calls.startNodeTask).toHaveBeenLastCalledWith({ action: "converge", params: { userRcon: "true" }, confirmMaintenance: true }));
  });
});

describe("server forwarding", () => {
  it("warns before legacy forwarding and confirms it", async () => {
    render(<ForwardingDialog serverName="legacy18" legacy={false} onUpdated={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /Forwarding mode/ }));
    expect(screen.queryByText(/trusts any identity/)).toBeNull();
    await userEvent.click(screen.getByRole("switch", { name: "Use legacy forwarding" }));
    expect(screen.getByText(/trusts any identity/)).toBeTruthy();
    await userEvent.click(await enabled("Save"));
    expect(calls.startNodeTask).toHaveBeenCalledWith({ action: "server-forwarding", params: { server: "legacy18", legacy: "true" }, confirmMaintenance: true });
  });
});
