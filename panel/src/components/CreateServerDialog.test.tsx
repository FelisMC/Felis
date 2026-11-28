// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, beforeAll } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CreateServerDialog } from "./CreateServerDialog";
import { humanizeError } from "@/lib/api";
import { FeaturesContext } from "@/lib/features";
import type { CreateServerRequest, PlatformFeaturesSettings, PlatformUsage, WhitelistImage } from "@/lib/types";

const tier = vi.hoisted(() => ({ identity: { user_id: "u-admin", email: "admin@example.test", role: "admin", is_admin: true, is_owner: false }, isOwner: false }));
const calls = vi.hoisted(() => ({ listImages: vi.fn(), createServer: vi.fn(), nodes: vi.fn(), fleetUsage: vi.fn() }));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

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

const usageWithout: PlatformUsage = { current: null, history: [], nodes: [], loginFailures24h: 0 };

beforeEach(() => {
  calls.listImages.mockReset();
  calls.createServer.mockReset();
  calls.fleetUsage.mockReset();
  onCreated.mockReset();
  calls.listImages.mockResolvedValue([image(PAPER, true), image(RETIRED, false)]);
  calls.createServer.mockResolvedValue({});
  calls.fleetUsage.mockResolvedValue(usageWithout);
});

async function openDialog(features?: PlatformFeaturesSettings) {
  const user = userEvent.setup();
  const node = <CreateServerDialog cfg={cfg} onCreated={onCreated} />;
  render(
    features ? (
      <FeaturesContext.Provider value={{ features, loaded: true, apply: vi.fn(), reload: vi.fn() }}>
        {node}
      </FeaturesContext.Provider>
    ) : (
      node
    ),
  );
  await user.click(screen.getByRole("button", { name: "New server" }));
  await screen.findByRole("dialog");
  if (!features || features.customImages) {
    await user.click(screen.getByRole("radio", { name: "Whitelisted image" }));
  }
  return user;
}

const titleBox = () => screen.getByLabelText("Server name") as HTMLInputElement;
const editIdentityBtn = () => screen.getByRole("button", { name: /Edit ID & address|Hide ID & address/ });
const nameBox = () => screen.getByLabelText("Name") as HTMLInputElement;
const subdomainBox = () => screen.getByLabelText("Subdomain") as HTMLInputElement;
const create = () => screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;

async function openIdentity(user: ReturnType<typeof userEvent.setup>) {
  if (!screen.queryByLabelText("Name")) {
    await user.click(editIdentityBtn());
  }
}

async function pick(user: ReturnType<typeof userEvent.setup>, field: string, option: string) {
  await user.click(screen.getByRole("combobox", { name: field }));
  await user.click(await screen.findByRole("option", { name: option }));
}

async function fillValid(user: ReturnType<typeof userEvent.setup>) {
  await user.type(titleBox(), "survival");
  await pick(user, "Image", PAPER);
}

function sent(): CreateServerRequest {
  expect(calls.createServer).toHaveBeenCalledTimes(1);
  return calls.createServer.mock.calls[0][0];
}

describe("CreateServerDialog request body", () => {
  it("auto-derives slug and subdomain from Server name and lets users override them inline", async () => {
    const user = await openDialog();

    await user.type(titleBox(), "  Survival World  ");
    expect(screen.getByText(/ID survival-world · Address survival-world\.example\.test/)).toBeTruthy();
    expect(screen.queryByLabelText("Name")).toBeNull();

    await openIdentity(user);
    expect(nameBox().value).toBe("survival-world");
    expect(subdomainBox().value).toBe("survival-world");

    await user.clear(nameBox());
    await user.type(nameBox(), "Survival-2");
    await user.clear(subdomainBox());
    await user.type(subdomainBox(), "SMP");
    await pick(user, "Image", PAPER);
    await user.clear(screen.getByLabelText("Memory"));
    await user.type(screen.getByLabelText("Memory"), "8GB");
    await user.clear(screen.getByLabelText("Storage"));
    await user.type(screen.getByLabelText("Storage"), "120GB");
    await pick(user, "Autostart policy", "Public — any player join wakes it");
    await user.click(create());

    expect(sent()).toStrictEqual({
      name: "survival-2",
      subdomain: "smp",
      displayName: "Survival World",
      image: PAPER,
      memory: "8Gi",
      storage: "120Gi",
      autostartPolicy: "public",
      ownerId: "u-admin",
    });
    expect(onCreated).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("auto-opens ID fields for non-ASCII display names and accepts MiB/MB/GiB/GB unit input without preset buttons", async () => {
    const user = await openDialog();
    await user.type(titleBox(), "生存一服");
    expect(nameBox().value).toBe("");
    await user.type(nameBox(), "survival");
    expect(subdomainBox().value).toBe("survival");
    await pick(user, "Image", PAPER);

    await user.clear(screen.getByLabelText("Memory"));
    await user.type(screen.getByLabelText("Memory"), "2048MB");
    await user.clear(screen.getByLabelText("Storage"));
    await user.type(screen.getByLabelText("Storage"), "10240MiB");
    await user.click(create());

    expect(sent()).toMatchObject({
      name: "survival",
      subdomain: "survival",
      displayName: "生存一服",
      memory: "2Gi",
      storage: "10Gi",
    });
  });

  it("refuses a memory below 1 GiB and a storage that is not whole GiB", async () => {
    const user = await openDialog();
    await fillValid(user);
    await user.clear(screen.getByLabelText("Memory"));
    await user.type(screen.getByLabelText("Memory"), "512MB");
    expect(screen.getByText("Enter a number of at least 1, in GiB.")).toBeTruthy();
    expect(create().disabled).toBe(true);
    await user.clear(screen.getByLabelText("Memory"));
    await user.type(screen.getByLabelText("Memory"), "1536MiB");
    await user.clear(screen.getByLabelText("Storage"));
    await user.type(screen.getByLabelText("Storage"), "7.5GB");
    expect(screen.getByText("Enter a whole number of at least 1, in GiB.")).toBeTruthy();
    expect(create().disabled).toBe(true);
    await user.clear(screen.getByLabelText("Storage"));
    await user.type(screen.getByLabelText("Storage"), "7");
    await user.click(create());
    expect(sent()).toMatchObject({ memory: "1536Mi", storage: "7Gi" });
  });

  it("stops the display name at the length the API takes", async () => {
    const user = await openDialog();

    await user.type(titleBox(), "x".repeat(70));
    await openIdentity(user);
    await user.clear(nameBox());
    await user.type(nameBox(), "survival");
    await user.clear(subdomainBox());
    await user.type(subdomainBox(), "survival");
    await pick(user, "Image", PAPER);
    await user.click(create());

    expect(sent().displayName).toBe("x".repeat(64));
  });

  it("omits redundant displayName when Server name equals the slug, and gives the server to its creator on the wake-list policy", async () => {
    const user = await openDialog();

    await fillValid(user);
    await user.click(create());

    expect(sent()).toStrictEqual({
      name: "survival",
      subdomain: "survival",
      displayName: undefined,
      image: PAPER,
      memory: "4Gi",
      storage: "10Gi",
      autostartPolicy: "allowlist",
      ownerId: "u-admin",
    });
  });

  it("creates an unclaimed server when nobody is chosen as its owner", async () => {
    const user = await openDialog();
    await fillValid(user);

    await pick(user, "Server owner", "Nobody — a player can claim it");
    expect(screen.getByText("Until a player claims the server, it gets no scheduled restore points.")).toBeTruthy();
    await user.click(create());

    expect(sent()).not.toHaveProperty("ownerId");
  });

  it("in team mode hides Owner and image source when customImages is off, defaults autostart to public, and uses manual-start copy", async () => {
    const user = await openDialog({
      mode: "team",
      useLobby: false,
      lobbyAvailable: false,
      customImages: false,
      allowSubmissions: false,
      requireAccountLink: false,
      revision: "r1",
    });

    expect(screen.queryByRole("radio", { name: "Whitelisted image" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Server owner" })).toBeNull();
    expect(calls.listImages).not.toHaveBeenCalled();

    const policyTrigger = screen.getByRole("combobox", { name: "Autostart policy" });
    expect(policyTrigger.textContent).toContain("Public");
    await user.click(policyTrigger);
    expect(await screen.findByRole("option", { name: "Manual start only — do not wake on join" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: /Owner only/ })).toBeNull();
  });

  it("starts the memory at what the host can still allocate, and warns past it without refusing", async () => {
    calls.fleetUsage.mockResolvedValue({ ...usageWithout, freeMemory: [{ node: "host", bytes: 1920 * 2 ** 20 }] });
    const user = await openDialog();
    const memory = screen.getByLabelText("Memory") as HTMLInputElement;

    await waitFor(() => expect(memory.value).toBe("1.5"));
    expect(screen.getByText("In GiB; decimals are allowed. 1.8 GiB can still be allocated.")).toBeTruthy();
    await user.clear(memory);
    await user.type(memory, "4");
    expect(screen.getByText("Only 1.8 GiB can still be allocated, so the server cannot start until memory is freed.")).toBeTruthy();
    await fillValid(user);
    await user.click(create());

    expect(sent().memory).toBe("4Gi");
  });

  it("says a host under 4 GB runs only the platform and offers no create", async () => {
    calls.fleetUsage.mockResolvedValue({ ...usageWithout, freeMemory: [{ node: "host", bytes: 2 ** 30, capacity: 1700 * 2 ** 20 }] });
    const user = await openDialog();

    expect(await screen.findByText("This host has under 4 GB of memory and runs only the platform. Game servers need a host with at least 4 GB of memory, or a worker node.")).toBeTruthy();
    await fillValid(user);
    expect(create().disabled).toBe(true);
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
    expect(titleBox().value).toBe("survival");
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
    ["lobby", "“lobby” is reserved by the platform. Use a different name."],
    ["craft-hl", "“craft-hl” is reserved by the platform. Use a different name."],
  ])("refuses the name %j before sending, and says why", async (name, why) => {
    const user = await openDialog();
    await fillValid(user);
    expect(create().disabled).toBe(false);

    await openIdentity(user);
    await user.clear(nameBox());
    await user.type(nameBox(), name);

    expect(create().disabled).toBe(true);
    expect(nameBox().getAttribute("aria-invalid")).toBe("true");
    const hintId = nameBox().getAttribute("aria-describedby");
    expect(hintId).toBeTruthy();
    expect(document.getElementById(hintId!)?.textContent).toBe(why);
  });

  it("holds the subdomain to the same rule", async () => {
    const user = await openDialog();
    await fillValid(user);

    await openIdentity(user);
    await user.clear(subdomainBox());
    await user.type(subdomainBox(), "api");

    expect(create().disabled).toBe(true);
    expect(screen.getByText("“api” is reserved by the platform. Use a different name.")).toBeTruthy();
    expect(screen.queryByText(/Will be reachable at/)).toBeNull();
  });

  it("shows where a good subdomain will be reachable", async () => {
    const user = await openDialog();

    await openIdentity(user);
    await user.type(subdomainBox(), "smp");

    expect(screen.getByText(/Will be reachable at smp\.example\.test/)).toBeTruthy();
    expect(subdomainBox().getAttribute("aria-invalid")).toBe("false");
  });
});

describe("CreateServerDialog on reopening", () => {
  const cancel = () => screen.getByRole("button", { name: "Cancel" });
  const reopen = async (user: ReturnType<typeof userEvent.setup>) => {
    await user.click(screen.getByRole("button", { name: "New server" }));
    await screen.findByRole("dialog");
  };

  it("rereads the image whitelist each time it opens", async () => {
    const NEW = "registry.example.test/fabric:1.21";
    const user = await openDialog();
    await user.click(cancel());
    expect(calls.listImages).toHaveBeenCalledTimes(1);

    calls.listImages.mockResolvedValue([image(PAPER, true), image(NEW, true)]);
    await reopen(user);
    expect(calls.listImages).toHaveBeenCalledTimes(2);
    await user.click(screen.getByRole("combobox", { name: "Image" }));
    const options = await screen.findAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual([PAPER, NEW]);
  });

  it("drops a chosen image that was disabled while the dialog was closed", async () => {
    const user = await openDialog();
    await fillValid(user);
    expect(create().disabled).toBe(false);
    await user.click(cancel());

    calls.listImages.mockResolvedValue([image(PAPER, false)]);
    await reopen(user);
    expect(await screen.findByText("No whitelisted images")).toBeTruthy();
    expect(titleBox().value).toBe("survival");
    expect(create().disabled).toBe(true);
  });

  it("clears a refusal once the form is edited, and when the dialog opens again", async () => {
    const refusal = { status: 409, code: "conflict", message: "server survival already exists" };
    calls.createServer.mockRejectedValue(refusal);
    const user = await openDialog();
    await fillValid(user);

    await user.click(create());
    expect(screen.getByText(humanizeError(refusal))).toBeTruthy();
    await user.type(titleBox(), "-2");
    expect(screen.queryByText(humanizeError(refusal))).toBeNull();

    await user.click(create());
    expect(screen.getByText(humanizeError(refusal))).toBeTruthy();
    await user.click(cancel());
    await reopen(user);
    expect(screen.queryByText(humanizeError(refusal))).toBeNull();
  });

  it("says when the whitelist could not be read, and reads it again on request", async () => {
    const outage = { status: 503, code: "unavailable", message: "images unavailable" };
    calls.listImages.mockRejectedValueOnce(outage);
    const user = await openDialog();

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(humanizeError(outage));
    expect(screen.getByText("Image list unavailable")).toBeTruthy();
    expect(screen.queryByText("No whitelisted images")).toBeNull();

    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(calls.listImages).toHaveBeenCalledTimes(2);
    await pick(user, "Image", PAPER);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("distributed placement", () => {
  it("requires an online approved worker and sends its name", async () => {
    calls.nodes.mockResolvedValue([
      { name: "b", role: "worker", ready: true, approved: true },
      { name: "c", role: "worker", ready: false, approved: true },
      { name: "a", role: "controller", ready: true, approved: true },
    ]);
    const user = userEvent.setup();
    render(<CreateServerDialog cfg={{ ...cfg, distributed: true }} onCreated={onCreated} />);
    await user.click(screen.getByRole("button", { name: "New server" }));
    await user.click(screen.getByRole("radio", { name: "Whitelisted image" }));
    await fillValid(user);
    expect(create().disabled).toBe(true);
    await user.click(screen.getByRole("combobox", { name: "Execution node" }));
    expect(screen.queryByRole("option", { name: "c" })).toBeNull();
    expect(screen.queryByRole("option", { name: "a" })).toBeNull();
    await user.click(await screen.findByRole("option", { name: "b" }));
    await user.click(create());
    expect(sent().nodeName).toBe("b");
  });
});
