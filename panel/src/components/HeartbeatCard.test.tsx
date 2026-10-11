// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HeartbeatCard } from "./HeartbeatCard";

const calls = vi.hoisted(() => ({ getHeartbeat: vi.fn(), setHeartbeat: vi.fn(), clearHeartbeat: vi.fn() }));
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

beforeEach(() => {
  vi.clearAllMocks();
  calls.getHeartbeat.mockResolvedValue({ set: false, display: "", revision: "rev-0" });
});

describe("monitoring heartbeat", () => {
  it("saves a URL with the revision, never shows its key back, and refuses one that is not http(s)", async () => {
    const user = userEvent.setup();
    calls.setHeartbeat.mockResolvedValue({ set: true, display: "https://hc-ping.com/...", revision: "rev-1" });
    render(<HeartbeatCard />);
    expect(await screen.findByText("Not set in the panel")).toBeTruthy();
    const save = screen.getByRole("button", { name: "Save heartbeat URL" }) as HTMLButtonElement;
    const field = screen.getByLabelText("Heartbeat URL") as HTMLInputElement;
    await waitFor(() => expect(field.disabled).toBe(false));
    expect(save.disabled).toBe(true);
    await user.type(field, "hc-ping.com/secret");
    expect(save.disabled).toBe(true);
    expect(field.getAttribute("aria-invalid")).toBe("true");
    await user.clear(field);
    await user.type(field, "ftp://hc-ping.com/secret");
    expect(save.disabled).toBe(true);
    await user.clear(field);
    await user.type(field, "https://hc-ping.com/secret-key");
    expect(save.disabled).toBe(false);
    await user.click(save);
    await waitFor(() => expect(calls.setHeartbeat).toHaveBeenCalledWith("https://hc-ping.com/secret-key", "rev-0"));
    expect(await screen.findByText("Current URL: https://hc-ping.com/...")).toBeTruthy();
    expect((screen.getByLabelText("Replace with a new heartbeat URL") as HTMLInputElement).value).toBe("");
    expect(document.body.textContent).not.toContain("secret-key");
  });

  it("removes the URL only after confirming", async () => {
    const user = userEvent.setup();
    calls.getHeartbeat.mockResolvedValue({ set: true, display: "https://hc-ping.com/...", revision: "rev-1" });
    calls.clearHeartbeat.mockResolvedValue({ set: false, display: "", revision: "rev-2" });
    render(<HeartbeatCard />);
    await user.click(await screen.findByRole("button", { name: "Remove heartbeat URL" }));
    expect(calls.clearHeartbeat).not.toHaveBeenCalled();
    expect(screen.getByText(/the monitoring service alerts on the missing pings/)).toBeTruthy();
    await user.click(screen.getAllByRole("button", { name: "Remove heartbeat URL" }).at(-1)!);
    await waitFor(() => expect(calls.clearHeartbeat).toHaveBeenCalled());
    expect(await screen.findByText("Not set in the panel")).toBeTruthy();
  });
});
