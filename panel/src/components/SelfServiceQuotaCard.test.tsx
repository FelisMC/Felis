// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SelfServiceQuotaCard } from "./SelfServiceQuotaCard";
import type { SelfServiceQuotaSettings } from "@/lib/types";

// A stand-in for GET/PUT /settings/self-service that stores what it is given and
// refuses a stale revision, as the API does.
const store = vi.hoisted(() => {
  const s = { saved: null as unknown as SelfServiceQuotaSettings, n: 0 };
  return {
    s,
    calls: {
      getSelfServiceQuota: vi.fn(async () => s.saved),
      setSelfServiceQuota: vi.fn(async (body: SelfServiceQuotaSettings) => {
        if (body.revision !== s.saved.revision) return Promise.reject({ status: 409, code: "conflict", message: "changed" });
        s.saved = { ...body, revision: `rev-${++s.n}` };
        return s.saved;
      }),
    },
  };
});
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...store.calls } };
});

beforeEach(() => {
  store.s.saved = { enabled: false, max_servers: null, max_cpu_milli: null, max_memory_mb: null, max_storage_gb: null, revision: "rev-0" };
});

async function ready() {
  const user = userEvent.setup();
  render(<SelfServiceQuotaCard />);
  await screen.findByText("Off");
  return user;
}
const box = (label: string) => screen.getByLabelText(label) as HTMLInputElement;

describe("default quota", () => {
  it("starts off, with the caps waiting for it to be turned on", async () => {
    await ready();
    expect(box("Servers").disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save default quota" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("saves memory in GB and CPU in cores as the quota's units, and an empty cap as unlimited", async () => {
    const user = await ready();
    await user.click(screen.getByRole("switch", { name: /Enable self-service servers/ }));
    await user.type(box("Servers"), "1");
    await user.type(box("Memory (GB)"), "4");
    await user.type(box("CPU (cores)"), "1.5");
    await user.click(screen.getByRole("button", { name: "Save default quota" }));
    await screen.findByText("The default quota has been saved.");
    expect(store.s.saved).toMatchObject({ enabled: true, max_servers: 1, max_memory_mb: 4096, max_cpu_milli: 1500, max_storage_gb: null });

    // Read back, the card shows the caps as they were typed.
    cleanup();
    render(<SelfServiceQuotaCard />);
    await screen.findByText("On");
    await waitFor(() => expect(box("Memory (GB)").value).toBe("4"));
    expect(box("CPU (cores)").value).toBe("1.5");
    expect(box("Storage (GB)").value).toBe("");
  });

  it("refuses a fractional server count before saving", async () => {
    const user = await ready();
    await user.click(screen.getByRole("switch", { name: /Enable self-service servers/ }));
    await user.type(box("Servers"), "1.5");
    expect(screen.getByText("Enter non-negative numbers; the server count must be a whole number.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save default quota" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
