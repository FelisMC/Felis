// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, beforeAll } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CreateServerDialog } from "./CreateServerDialog";
import { humanizeError } from "@/lib/api";
import type { CreateServerRequest, WhitelistImage } from "@/lib/types";

const calls = vi.hoisted(() => ({ listImages: vi.fn(), createServer: vi.fn() }));
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

const PAPER = "registry.example.test/paper:1.21";
const RETIRED = "registry.example.test/paper:1.20";
const cfg = { apiBase: "/api/v1", rootDomain: "example.test", gamePort: 25570 };
const onCreated = vi.fn();

function image(image_ref: string, enabled: boolean): WhitelistImage {
  return { image_ref, enabled } as WhitelistImage;
}

beforeEach(() => {
  calls.listImages.mockReset();
  calls.createServer.mockReset();
  onCreated.mockReset();
  calls.listImages.mockResolvedValue([image(PAPER, true), image(RETIRED, false)]);
  calls.createServer.mockResolvedValue({});
});

async function openDialog() {
  const user = userEvent.setup();
  render(<CreateServerDialog cfg={cfg} onCreated={onCreated} />);
  await user.click(screen.getByRole("button", { name: "New server" }));
  await screen.findByRole("dialog");
  return user;
}

const nameBox = () => screen.getByLabelText("Name") as HTMLInputElement;
const subdomainBox = () => screen.getByLabelText("Subdomain") as HTMLInputElement;
const create = () => screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;

async function pick(user: ReturnType<typeof userEvent.setup>, field: string, option: string) {
  await user.click(screen.getByRole("combobox", { name: field }));
  await user.click(await screen.findByRole("option", { name: option }));
}

/** fillValid fills every required field with what the API takes. */
async function fillValid(user: ReturnType<typeof userEvent.setup>) {
  await user.type(nameBox(), "survival");
  await user.type(subdomainBox(), "survival");
  await pick(user, "Image", PAPER);
}

function sent(): CreateServerRequest {
  expect(calls.createServer).toHaveBeenCalledTimes(1);
  return calls.createServer.mock.calls[0][0];
}

describe("CreateServerDialog request body", () => {
  it("sends every field as chosen, lowercased and trimmed", async () => {
    const user = await openDialog();

    await user.type(nameBox(), "Survival-2");
    await user.type(subdomainBox(), "SMP");
    await user.type(screen.getByLabelText("Display name (optional)"), "  Survival World  ");
    await pick(user, "Image", PAPER);
    await pick(user, "Memory", "8Gi");
    await pick(user, "Storage", "20Gi");
    await pick(user, "Autostart policy", "Public — any player join wakes it");
    await user.click(create());

    expect(sent()).toStrictEqual({
      name: "survival-2",
      subdomain: "smp",
      displayName: "Survival World",
      image: PAPER,
      memory: "8Gi",
      storage: "20Gi",
      autostartPolicy: "public",
    });
    expect(onCreated).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("leaves a blank display name out, and sends the default sizes and policy", async () => {
    const user = await openDialog();

    await fillValid(user);
    await user.type(screen.getByLabelText("Display name (optional)"), "   ");
    await user.click(create());

    expect(sent()).toStrictEqual({
      name: "survival",
      subdomain: "survival",
      displayName: undefined,
      image: PAPER,
      memory: "4Gi",
      storage: "10Gi",
      autostartPolicy: "ownerOnly",
    });
  });

  it("offers only the enabled images", async () => {
    const user = await openDialog();

    await user.click(screen.getByRole("combobox", { name: "Image" }));
    const options = await screen.findAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual([PAPER]);
  });

  it("keeps the form and says why when the API refuses, then creates on the next try", async () => {
    const refusal = { status: 409, code: "conflict", message: "server survival already exists" };
    calls.createServer.mockRejectedValueOnce(refusal);
    const user = await openDialog();

    await fillValid(user);
    await user.click(create());

    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(humanizeError(refusal))).toBeTruthy();
    expect(nameBox().value).toBe("survival");
    expect(onCreated).not.toHaveBeenCalled();

    await user.click(create());
    expect(calls.createServer).toHaveBeenCalledTimes(2);
    expect(onCreated).toHaveBeenCalledTimes(1);
  });
});

describe("CreateServerDialog names", () => {
  it.each([
    ["ab", "Use 3–32 lowercase letters, digits or hyphens, with no hyphen at either end."],
    ["-survival", "Use 3–32 lowercase letters, digits or hyphens, with no hyphen at either end."],
    ["survival-", "Use 3–32 lowercase letters, digits or hyphens, with no hyphen at either end."],
    ["my_world", "Use 3–32 lowercase letters, digits or hyphens, with no hyphen at either end."],
    ["a".repeat(33), "Use 3–32 lowercase letters, digits or hyphens, with no hyphen at either end."],
    ["lobby", "“lobby” is kept for the platform. Pick another."],
  ])("refuses the name %j before sending, and says why", async (name, why) => {
    const user = await openDialog();
    await fillValid(user);
    expect(create().disabled).toBe(false);

    await user.clear(nameBox());
    await user.type(nameBox(), name);

    expect(create().disabled).toBe(true);
    expect(nameBox().getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText(why)).toBeTruthy();
  });

  it("holds the subdomain to the same rule", async () => {
    const user = await openDialog();
    await fillValid(user);

    await user.clear(subdomainBox());
    await user.type(subdomainBox(), "api");

    expect(create().disabled).toBe(true);
    expect(screen.getByText("“api” is kept for the platform. Pick another.")).toBeTruthy();
    expect(screen.queryByText(/Will be reachable at/)).toBeNull();
  });

  it("shows where a good subdomain will be reachable", async () => {
    const user = await openDialog();

    await user.type(subdomainBox(), "smp");

    expect(screen.getByText(/Will be reachable at smp\.example\.test/)).toBeTruthy();
    expect(subdomainBox().getAttribute("aria-invalid")).toBe("false");
  });
});
