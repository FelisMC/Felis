// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, beforeAll } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CreateServerDialog } from "./CreateServerDialog";
import { EditServerDialog } from "./EditServerDialog";
import type { CreateServerRequest, SelfCreateRequest, ServerQuota, ServerSoftware, SoftwareRequest, SoftwareType } from "@/lib/types";

// A stand-in for felis-api's software routes and create/patch, answering the way
// the real ones do: versions newest first with pre-releases marked, a version's
// builds with the recommended one flagged and the Java it needs, refusals for an
// older Java, a missing EULA, an unconfirmed version move or a full quota.
const backend = vi.hoisted(() => {
  type Server = { software?: ServerSoftware; image?: string; memory: string; owner?: string };
  const versions: Record<string, { id: string; stable: boolean }[]> = {
    paper: [{ id: "1.21.5-rc1", stable: false }, { id: "1.21.4", stable: true }, { id: "1.12.2", stable: true }],
    fabric: [{ id: "1.21.4", stable: true }, { id: "1.20.1", stable: true }],
    vanilla: [{ id: "26.3", stable: true }, { id: "1.21.4", stable: true }],
  };
  const java: Record<string, number> = { "1.21.4": 21, "1.21.5-rc1": 21, "1.20.1": 17, "1.12.2": 8, "26.3": 25 };
  const builds: Record<string, { id: string; stable: boolean; recommended?: boolean }[]> = {
    paper: [{ id: "233", stable: false }, { id: "232", stable: true, recommended: true }, { id: "231", stable: true }],
    fabric: [{ id: "0.20.0", stable: false }, { id: "0.19.5", stable: true, recommended: true }],
  };
  const forwarding = (type: string, mc: string) =>
    type === "vanilla" || (type === "paper" && mc === "1.12.2") ? "none" : "velocity";
  const state = { servers: {} as Record<string, Server>, quota: null as ServerQuota | null };
  const err = (code: string) => Promise.reject({ status: 400, code, message: code });
  function resolve(req: SoftwareRequest): ServerSoftware | string {
    if (req.type === "custom") return { type: "custom", jar: req.jar, java: req.java ?? 21, forwarding: "none" };
    const mc = req.minecraftVersion ?? "";
    if (!versions[req.type]?.some((v) => v.id === mc)) return "software_not_found";
    const need = java[mc];
    if (req.java && req.java < need) return "java_too_old";
    const runtime = ([8, 17, 21, 25] as const).find((r) => r >= need)!;
    const b = builds[req.type] ? req.build ?? builds[req.type].find((x) => x.recommended)!.id : undefined;
    return { type: req.type, minecraftVersion: mc, build: b, java: req.java ?? runtime, forwarding: forwarding(req.type, mc) };
  }
  return {
    state,
    api: {
      softwareVersions: vi.fn(async (type: SoftwareType) => versions[type] ?? []),
      softwareVersion: vi.fn(async (type: SoftwareType, mc: string) => {
        const need = java[mc];
        return { version: mc, requiredJava: need, java: [8, 17, 21, 25].find((r) => r >= need), builds: builds[type] ?? [], forwarding: forwarding(type, mc) };
      }),
      listImages: vi.fn(async () => []),
      nodes: vi.fn(async () => []),
      createServer: vi.fn(async (req: CreateServerRequest) => {
        if (req.software && !req.acceptEula) return err("eula_required");
        const sw = req.software ? resolve(req.software) : undefined;
        if (typeof sw === "string") return err(sw);
        state.servers[req.name] = { software: sw, image: req.image, memory: req.memory };
        return { name: req.name, subdomain: req.subdomain, desiredState: "Stopped" };
      }),
      createMyServer: vi.fn(async (req: SelfCreateRequest) => {
        if (!req.acceptEula) return err("eula_required");
        const q = state.quota!;
        const mem = Number(req.memory.replace("Gi", "")) * 1024;
        if (!q.canCreate || (q.limits?.max_memory_mb != null && q.usage.memory_mb + mem > q.limits.max_memory_mb)) return err("quota_exceeded");
        const sw = resolve(req.software);
        if (typeof sw === "string") return err(sw);
        state.servers[req.name] = { software: sw, memory: req.memory, owner: "me" };
        return { name: req.name, subdomain: req.subdomain, desiredState: "Stopped" };
      }),
      patchMyServer: vi.fn(async (name: string, req: { software?: SoftwareRequest; confirmSoftwareChange?: boolean }) => {
        const cur = state.servers[name];
        if (req.software) {
          const sw = resolve(req.software);
          if (typeof sw === "string") return err(sw);
          const moves = !cur.software || cur.software.type !== sw.type || cur.software.minecraftVersion !== sw.minecraftVersion;
          if (moves && !req.confirmSoftwareChange) return Promise.reject({ status: 409, code: "software_change_unconfirmed", message: "" });
          cur.software = sw;
        }
        return { name, patched: req.software ? ["software"] : [] };
      }),
    },
  };
});
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...backend.api } };
});

beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

const cfg = { apiBase: "/api/v1", rootDomain: "example.test", gamePort: 25570 };

beforeEach(() => {
  backend.state.servers = {};
  backend.state.quota = null;
});

type User = ReturnType<typeof userEvent.setup>;

async function choose(user: User, field: string, option: string | RegExp) {
  await user.click(screen.getByRole("combobox", { name: field }));
  await user.click(await screen.findByRole("option", { name: option }));
}

async function options(user: User, field: string): Promise<string[]> {
  await user.click(screen.getByRole("combobox", { name: field }));
  const list = await screen.findByRole("listbox");
  const names = within(list).getAllByRole("option").map((o) => o.textContent ?? "");
  await user.keyboard("{Escape}");
  return names;
}

const eula = () => screen.getByRole("checkbox", { name: /accept the Minecraft End User License Agreement/ }) as HTMLInputElement;
const create = () => screen.getByRole("button", { name: "Create" }) as HTMLButtonElement;

async function openStaff() {
  const user = userEvent.setup();
  const onCreated = vi.fn();
  render(<CreateServerDialog cfg={cfg} onCreated={onCreated} />);
  await user.click(screen.getByRole("button", { name: "New server" }));
  await screen.findByRole("dialog");
  return { user, onCreated };
}

describe("choosing server software", () => {
  it("lists versions newest first and holds pre-releases back until asked", async () => {
    const { user } = await openStaff();
    await user.click(screen.getByRole("radio", { name: "Paper" }));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Game version" })).not.toHaveProperty("disabled", true));
    expect(await options(user, "Game version")).toEqual(["1.21.4", "1.12.2"]);
    await user.click(screen.getByRole("checkbox", { name: "Show pre-releases and snapshots" }));
    expect(await options(user, "Game version")).toEqual(["1.21.5-rc1 (pre-release)", "1.21.4", "1.12.2"]);
  });

  it("preselects the recommended build and the Java the version needs, and offers no older Java", async () => {
    const { user } = await openStaff();
    await user.click(screen.getByRole("radio", { name: "Fabric" }));
    await choose(user, "Game version", "1.20.1");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Loader version" }).textContent).toBe("0.19.5 (recommended)"));
    expect(screen.getByRole("combobox", { name: "Java version" }).textContent).toBe("Java 17 (recommended)");
    expect(await options(user, "Java version")).toEqual(["Java 17 (recommended)", "Java 21", "Java 25"]);
    expect(screen.getByText("This game version requires Java 17 or later.")).toBeTruthy();
  });

  it("says before creating when players will join without their identity", async () => {
    const { user } = await openStaff();
    await user.click(screen.getByRole("radio", { name: "Vanilla" }));
    await choose(user, "Game version", "26.3");
    expect(await screen.findByRole("note")).toHaveProperty("textContent", expect.stringContaining("offline-mode UUIDs"));
    await user.click(screen.getByRole("radio", { name: "Paper" }));
    await choose(user, "Game version", "1.21.4");
    expect(await screen.findByText(/join through the proxy with their premium identity/)).toBeTruthy();
    expect(screen.queryByRole("note")).toBeNull();
  });

  it("creates the server with what was picked, once the EULA is accepted", async () => {
    const { user, onCreated } = await openStaff();
    await user.type(screen.getByLabelText("Name"), "smp");
    await user.type(screen.getByLabelText("Subdomain"), "smp");
    await user.click(screen.getByRole("radio", { name: "Paper" }));
    await choose(user, "Game version", "1.21.4");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Build" }).textContent).toBe("232 (recommended)"));
    expect(create().disabled).toBe(true);
    await user.click(eula());
    expect(create().disabled).toBe(false);
    await user.click(create());

    await waitFor(() => expect(onCreated).toHaveBeenCalled());
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(backend.state.servers.smp.software).toEqual({ type: "paper", minecraftVersion: "1.21.4", build: "232", java: 21, forwarding: "velocity" });
  });

  it("takes a custom jar by a path inside the server directory only", async () => {
    const { user } = await openStaff();
    await user.type(screen.getByLabelText("Name"), "mine");
    await user.type(screen.getByLabelText("Subdomain"), "mine");
    await user.click(screen.getByRole("radio", { name: "Custom jar" }));
    await user.click(eula());
    await user.type(screen.getByLabelText("Jar path"), "../server.jar");
    expect(screen.getByText(/relative path inside the server directory/)).toBeTruthy();
    expect(create().disabled).toBe(true);
    // .felis holds the jars the platform installs and replaces on every start.
    await user.clear(screen.getByLabelText("Jar path"));
    await user.type(screen.getByLabelText("Jar path"), ".felis/server/paper.jar");
    expect(screen.getByText(/outside the \.felis directory/)).toBeTruthy();
    expect(create().disabled).toBe(true);
    await user.clear(screen.getByLabelText("Jar path"));
    await user.type(screen.getByLabelText("Jar path"), "pack/start.jar");
    await user.click(create());
    await waitFor(() => expect(backend.state.servers.mine?.software).toEqual({ type: "custom", jar: "pack/start.jar", java: 21, forwarding: "none" }));
  });
});

describe("a player creating their own server", () => {
  const quota = (over: Partial<ServerQuota> = {}): ServerQuota => ({
    source: "default", limits: { max_servers: 2, max_memory_mb: 6144 }, usage: { servers: 1, cpu_milli: 0, memory_mb: 2048, storage_mb: 10240 }, canCreate: true, ...over,
  });

  it("shows what is left of the quota and offers no size past it", async () => {
    backend.state.quota = quota();
    const user = userEvent.setup();
    render(<CreateServerDialog cfg={cfg} self={{ quota: backend.state.quota }} onCreated={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: "New server" }));
    expect(await screen.findByText("Quota usage: servers 1 / 2, memory 2048 / 6144 MB")).toBeTruthy();
    expect(screen.queryByRole("radio", { name: "Whitelisted image" })).toBeNull();
    await user.click(screen.getByRole("combobox", { name: "Memory" }));
    const six = await screen.findByRole("option", { name: "6Gi (exceeds the remaining quota)" });
    expect(six.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByRole("option", { name: "4Gi" }).getAttribute("aria-disabled")).not.toBe("true");
  });

  it("creates a server the player then owns", async () => {
    backend.state.quota = quota();
    const user = userEvent.setup();
    const onCreated = vi.fn();
    render(<CreateServerDialog cfg={cfg} self={{ quota: backend.state.quota }} onCreated={onCreated} />);
    await user.click(screen.getByRole("button", { name: "New server" }));
    await user.type(await screen.findByLabelText("Name"), "mine");
    await user.type(screen.getByLabelText("Subdomain"), "mine");
    await user.click(screen.getByRole("radio", { name: "Fabric" }));
    await choose(user, "Game version", "1.21.4");
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Loader version" }).textContent).toContain("0.19.5"));
    await user.click(eula());
    await user.click(create());
    await waitFor(() => expect(onCreated).toHaveBeenCalled());
    expect(backend.state.servers.mine).toMatchObject({ owner: "me", software: { type: "fabric", minecraftVersion: "1.21.4", build: "0.19.5", java: 21 } });
  });

  it("cannot open the form with the quota used up, and says why", async () => {
    render(<CreateServerDialog cfg={cfg} self={{ quota: quota({ canCreate: false, reason: "quota_full" }) }} onCreated={vi.fn()} />);
    const button = screen.getByRole("button", { name: "New server" }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(button.title).toBe("The server quota is fully used. New servers cannot be created.");
  });
});

describe("an owner changing their server's software", () => {
  const current: ServerSoftware = { type: "paper", minecraftVersion: "1.21.4", build: "232", java: 21, forwarding: "velocity" };

  async function openOwner() {
    backend.state.servers.mine = { software: { ...current }, memory: "4Gi", owner: "me" };
    const user = userEvent.setup();
    const onUpdated = vi.fn();
    render(
      <EditServerDialog serverName="mine" mode="owner" currentSoftware={current} currentMemory="4Gi" currentStorage="10Gi" currentIdleStopSeconds={600} onUpdated={onUpdated} />,
    );
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await screen.findByRole("dialog");
    return { user, onUpdated };
  }
  const save = () => screen.getByRole("button", { name: "Save Config" }) as HTMLButtonElement;

  it("offers no image, and shows what the server runs", async () => {
    await openOwner();
    expect(screen.queryByRole("combobox", { name: "Image" })).toBeNull();
    expect(screen.getByText("Current: Paper 1.21.4 232, Java 21")).toBeTruthy();
  });

  it("asks for a backup acknowledgement before moving the world to another version", async () => {
    const { user, onUpdated } = await openOwner();
    await user.click(screen.getByRole("button", { name: "Change server software" }));
    await choose(user, "Game version", "1.12.2");
    await screen.findByText(/Upgraded chunks are not compatible with older versions/);
    expect(save().disabled).toBe(true);
    await user.click(screen.getByRole("checkbox", { name: "I've backed up the world — change the software" }));
    await user.click(save());
    await waitFor(() => expect(onUpdated).toHaveBeenCalled());
    expect(backend.state.servers.mine.software).toMatchObject({ type: "paper", minecraftVersion: "1.12.2", forwarding: "none" });
  });

  // The console rereads the status every few seconds and passes the software it
  // reports as a new object each time.
  it("keeps a choice being made while the status poll reports the same software", async () => {
    backend.state.servers.mine = { software: { ...current }, memory: "4Gi", owner: "me" };
    const user = userEvent.setup();
    const props = { serverName: "mine", mode: "owner" as const, currentMemory: "4Gi", currentStorage: "10Gi", currentIdleStopSeconds: 600, onUpdated: vi.fn() };
    const { rerender } = render(<EditServerDialog {...props} currentSoftware={{ ...current }} />);
    await user.click(screen.getByRole("button", { name: /Edit Server Config/ }));
    await user.click(await screen.findByRole("button", { name: "Change server software" }));
    await choose(user, "Game version", "1.12.2");
    const confirm = () => screen.getByRole("checkbox", { name: "I've backed up the world — change the software" }) as HTMLInputElement;
    await user.click(confirm());
    rerender(<EditServerDialog {...props} currentSoftware={{ ...current }} />);
    expect(screen.getByRole("combobox", { name: "Game version" }).textContent).toContain("1.12.2");
    expect(confirm().checked).toBe(true);
    expect(save().disabled).toBe(false);
  });

  it("changes the build of the same version without one", async () => {
    const { user, onUpdated } = await openOwner();
    await user.click(screen.getByRole("button", { name: "Change server software" }));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Build" }).textContent).toContain("232"));
    await choose(user, "Build", "231");
    expect(screen.queryByText(/Upgraded chunks are not compatible/)).toBeNull();
    await user.click(save());
    await waitFor(() => expect(onUpdated).toHaveBeenCalled());
    expect(backend.state.servers.mine.software?.build).toBe("231");
  });
});
