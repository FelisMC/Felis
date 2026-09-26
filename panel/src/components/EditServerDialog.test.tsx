// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, beforeAll } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EditServerDialog } from "./EditServerDialog";

const calls = vi.hoisted(() => ({ listImages: vi.fn(), patchServer: vi.fn() }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

// Radix Select opens with pointer capture and scrolls the picked item into view,
// neither of which jsdom implements.
beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const onUpdated = vi.fn();

beforeEach(() => {
  calls.listImages.mockReset();
  calls.patchServer.mockReset();
  onUpdated.mockReset();
  calls.listImages.mockResolvedValue([]);
  calls.patchServer.mockResolvedValue({ name: "survival", patched: [] });
});

async function openDialog() {
  const user = userEvent.setup();
  render(
    <EditServerDialog
      serverName="survival"
      currentDisplayName="Survival Realm"
      currentPolicy="ownerOnly"
      currentImage="registry.example.test/paper:1.21"
      currentMemory="4Gi"
      currentStorage="10Gi"
      currentCpu="1"
      currentIdleStopSeconds={600}
      onUpdated={onUpdated}
    />,
  );
  await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
  await screen.findByRole("dialog");
  return user;
}

const save = () => screen.getByRole("button", { name: "Save Config" }) as HTMLButtonElement;
const cpuInput = () => screen.getByLabelText("CPU Limit") as HTMLInputElement;

describe("EditServerDialog request body", () => {
  it("sends the CPU alone when only the CPU changed", async () => {
    const user = await openDialog();
    await user.clear(cpuInput());
    await user.type(cpuInput(), "2");
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { resources: { cpu: "2" } });
    expect(onUpdated).toHaveBeenCalledTimes(1);
  });

  it("sends the memory alone when only the memory changed", async () => {
    const user = await openDialog();
    await user.click(screen.getByRole("combobox", { name: "Memory" }));
    await user.click(await screen.findByRole("option", { name: "8Gi" }));
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { memory: "8Gi" });
  });

  it("clears the display name with an empty one", async () => {
    const user = await openDialog();
    await user.clear(screen.getByLabelText("Display name (optional)"));
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { displayName: "" });
  });

  it("lifts the CPU limit when the field is emptied", async () => {
    const user = await openDialog();
    await user.clear(cpuInput());
    await user.click(save());

    expect(calls.patchServer).toHaveBeenCalledWith("survival", { resources: { cpu: "" } });
  });

  it("treats surrounding space as no change", async () => {
    const user = await openDialog();
    await user.type(cpuInput(), "  ");
    await user.type(screen.getByLabelText("Display name (optional)"), " ");

    expect(save().disabled).toBe(true);
  });

  it("refuses a CPU value the server cannot take", async () => {
    const user = await openDialog();
    for (const bad of ["two", "0", "1.5m", "-1"]) {
      await user.clear(cpuInput());
      await user.type(cpuInput(), bad);
      expect(screen.getByText("Enter cores (1, 1.5) or millicores (500m), above zero."), bad).toBeTruthy();
      expect(cpuInput().getAttribute("aria-invalid"), bad).toBe("true");
      expect(save().disabled, bad).toBe(true);
    }
    for (const good of ["1.5", "500m", "4"]) {
      await user.clear(cpuInput());
      await user.type(cpuInput(), good);
      expect(screen.queryByText(/millicores/), good).toBeNull();
      expect(save().disabled, good).toBe(false);
    }
    expect(calls.patchServer).not.toHaveBeenCalled();
  });

  it("can be cancelled with nothing changed", async () => {
    const user = await openDialog();
    expect(save().disabled).toBe(true);
    const cancel = screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
    expect(cancel.disabled).toBe(false);
    await user.click(cancel);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
