// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, beforeAll } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { FleetServer, MyServerView } from "@/lib/types";
import { ServersPage } from "./ServersPage";

const tier = vi.hoisted(() => ({ isAdmin: true, identity: { email: "admin@example.test" } }));
const calls = vi.hoisted(() => ({ fleet: vi.fn(), myServers: vi.fn() }));

vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
vi.mock("@/lib/config", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/config")>();
  return {
    ...actual,
    loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "example.test", gamePort: 25570 }),
  };
});
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

function row(name: string, over: Partial<FleetServer>): FleetServer {
  return {
    name,
    subdomain: name,
    phase: "Stopped",
    ready: false,
    playersOnline: 0,
    playersMax: 20,
    owned: false,
    claimable: false,
    ...over,
  } as FleetServer;
}

// The desktop table row of one server (the phone cards render the same data).
async function tableRow(name: string) {
  const table = await screen.findByRole("table");
  const cell = within(table).getByText(name);
  const tr = cell.closest("tr");
  if (!tr) throw new Error(`no row for ${name}`);
  return within(tr);
}

// Radix Select opens with pointer capture and scrolls the picked item into view,
// neither of which jsdom implements.
beforeAll(() => {
  Element.prototype.hasPointerCapture ??= () => false;
  Element.prototype.releasePointerCapture ??= () => {};
  Element.prototype.scrollIntoView ??= () => {};
});

// The labels of the desktop table's rows, top to bottom.
async function tableOrder() {
  const table = await screen.findByRole("table");
  return within(table)
    .getAllByRole("row")
    .slice(1)
    .map((tr) => tr.querySelector("td span.font-medium")?.textContent);
}

function renderPage() {
  render(
    <MemoryRouter>
      <ServersPage />
    </MemoryRouter>,
  );
  return userEvent.setup();
}

async function sortBy(user: ReturnType<typeof userEvent.setup>, option: string) {
  await user.click(screen.getByRole("combobox", { name: "Sort order" }));
  await user.click(await screen.findByRole("option", { name: option }));
}

beforeEach(() => {
  tier.isAdmin = true;
  calls.fleet.mockReset();
  calls.myServers.mockReset();
});

describe("ServersPage fleet ownership", () => {
  it("trusts the server's ownership and claim flags over the owner text", async () => {
    calls.fleet.mockResolvedValue([
      // The caller has no email: its own server shows the username, which never
      // equals identity.email, yet the row is still marked as the caller's.
      row("survival", { owner: "steve-mc", owned: true }),
      row("creative", { owner: "alice@example.test" }),
      row("skyblock", { claimable: true }),
    ]);
    render(
      <MemoryRouter>
        <ServersPage />
      </MemoryRouter>,
    );

    const survival = await tableRow("survival");
    expect(survival.getByText("You")).toBeTruthy();
    expect(survival.getByText("steve-mc")).toBeTruthy();
    expect(survival.queryByRole("button", { name: /Claim/ })).toBeNull();

    const creative = await tableRow("creative");
    expect(creative.queryByText("You")).toBeNull();
    expect(creative.queryByRole("button", { name: /Claim/ })).toBeNull();

    const skyblock = await tableRow("skyblock");
    expect(skyblock.getByText("Unclaimed")).toBeTruthy();
    expect(skyblock.getByRole("button", { name: /Claim/ })).toBeTruthy();
  });

  it("says the owner is unknown and offers no claim when the lookup failed", async () => {
    calls.fleet.mockResolvedValue([row("survival", { ownerUnknown: true })]);
    render(
      <MemoryRouter>
        <ServersPage />
      </MemoryRouter>,
    );

    const survival = await tableRow("survival");
    expect(survival.getByText("Unknown")).toBeTruthy();
    expect(survival.queryByText("Unclaimed")).toBeNull();
    expect(survival.queryByRole("button", { name: /Claim/ })).toBeNull();
  });
});

describe("ServersPage addresses", () => {
  it("offers a player the join address to copy and no internal address", async () => {
    tier.isAdmin = false;
    const mine: MyServerView = {
      name: "survival",
      subdomain: "survival",
      owned: true,
      claimable: false,
      phase: "Running",
      playersOnline: 3,
      playersMax: 20,
    };
    calls.myServers.mockResolvedValue([mine]);
    render(
      <MemoryRouter>
        <ServersPage />
      </MemoryRouter>,
    );

    const table = await screen.findByRole("table");
    expect(within(table).queryByText("Internal address")).toBeNull();
    // The player's view has no endpoint, so a column for it could only say "Not running".
    expect(within(table).queryByText("Not running")).toBeNull();
    const survival = await tableRow("survival");
    expect(survival.getByRole("button", { name: "Copy address survival.example.test:25570" })).toBeTruthy();
    expect(survival.queryByRole("link", { name: /survival\.example\.test/ })).toBeNull();
  });

  it("keeps the proxy's internal address for admins", async () => {
    calls.fleet.mockResolvedValue([
      row("survival", { phase: "Running", ready: true, endpointAddress: "10.43.0.10:25565" }),
    ]);
    render(
      <MemoryRouter>
        <ServersPage />
      </MemoryRouter>,
    );

    const table = await screen.findByRole("table");
    expect(within(table).getByText("Internal address")).toBeTruthy();
    const survival = await tableRow("survival");
    expect(survival.getByText("10.43.0.10:25565")).toBeTruthy();
    expect(survival.getByRole("button", { name: "Copy address survival.example.test:25570" })).toBeTruthy();
  });
});

describe("ServersPage failed starts", () => {
  it("offers a failed start a retry and a stop, and tells retrying from given up", async () => {
    calls.fleet.mockResolvedValue([
      row("broken", { phase: "Failed", desiredState: "Running", startGaveUp: true, autoRestarts: 3, owned: true }),
      row("flaky", { phase: "Failed", desiredState: "Running", autoRestarts: 1, owned: true }),
    ]);
    render(
      <MemoryRouter>
        <ServersPage />
      </MemoryRouter>,
    );

    const broken = await tableRow("broken");
    expect(broken.getByText("Failed to start")).toBeTruthy();
    expect(broken.getByRole("button", { name: /Retry start/ })).toBeTruthy();
    expect(broken.getByRole("button", { name: /Stop/ })).toBeTruthy();
    expect(broken.queryByRole("button", { name: /Wake/ })).toBeNull();

    const flaky = await tableRow("flaky");
    expect(flaky.getByText("Timed out · retrying")).toBeTruthy();
    expect(flaky.getByRole("button", { name: /Retry start/ })).toBeTruthy();
  });
});

describe("ServersPage servers asked to move", () => {
  it("shows a wake or stop at once, in the badge, the button and the counts", async () => {
    calls.fleet.mockResolvedValue([
      row("survival", { phase: "Stopped", desiredState: "Running", owned: true }),
      row("creative", { phase: "Running", desiredState: "Stopped", ready: true, owned: true }),
    ]);
    render(
      <MemoryRouter>
        <ServersPage />
      </MemoryRouter>,
    );

    const survival = await tableRow("survival");
    expect(survival.getByText("Starting")).toBeTruthy();
    expect(survival.queryByText("Stopped")).toBeNull();
    // Stop stays offered: the way out of a start that never comes up.
    expect(survival.getByRole("button", { name: "Stop" })).toHaveProperty("disabled", false);

    const creative = await tableRow("creative");
    expect(creative.getByText("Stopping")).toBeTruthy();
    expect(creative.getByRole("button", { name: "Stopping…" })).toHaveProperty("disabled", true);
    expect(creative.queryByRole("button", { name: "Stop" })).toBeNull();

    // The Running card counts neither: one is not up yet, the other is going down.
    const runningCard = screen.getByText("Running", { selector: "div.mt-1" });
    expect(runningCard.previousElementSibling?.textContent).toBe("0");
  });
});

describe("ServersPage retiring servers", () => {
  it("shows a server given up or being deleted with its badge and no start", async () => {
    const requested_at = new Date().toISOString();
    calls.fleet.mockResolvedValue([
      row("survival", { owner: "steve-mc", retiring: { requested_at, delete: false } }),
      row("creative", { owner: "alice@example.test", retiring: { requested_at, delete: true } }),
      row("skyblock", { owner: "bob@example.test" }),
    ]);
    render(
      <MemoryRouter>
        <ServersPage />
      </MemoryRouter>,
    );

    const survival = await tableRow("survival");
    expect(survival.getByText("Given up")).toBeTruthy();
    expect(survival.queryByRole("button", { name: /Wake/ })).toBeNull();
    const creative = await tableRow("creative");
    expect(creative.getByText("Deleting")).toBeTruthy();
    expect(creative.queryByRole("button", { name: /Wake/ })).toBeNull();
    const skyblock = await tableRow("skyblock");
    expect(skyblock.getByRole("button", { name: /Wake/ })).toBeTruthy();
  });

  it("marks the owner's own server given up in their list", async () => {
    tier.isAdmin = false;
    calls.myServers.mockResolvedValue([
      {
        name: "survival",
        subdomain: "survival",
        owned: true,
        claimable: false,
        phase: "Stopped",
        desiredState: "Stopped",
        playersOnline: 0,
        playersMax: 20,
        retiring: { requested_at: new Date().toISOString(), delete: false },
      } satisfies MyServerView,
    ]);
    render(
      <MemoryRouter>
        <ServersPage />
      </MemoryRouter>,
    );

    const survival = await tableRow("survival");
    expect(survival.getByText("Given up")).toBeTruthy();
    expect(survival.queryByRole("button", { name: /Wake/ })).toBeNull();
  });
});

describe("ServersPage order and paging", () => {
  it("lists by name, and on request by status or by players online", async () => {
    calls.fleet.mockResolvedValue([
      row("survival", { phase: "Running", ready: true, playersOnline: 3 }),
      row("creative", { phase: "Stopped" }),
      row("skyblock", { phase: "Running", ready: true, playersOnline: 7 }),
      // Ordered by the label it shows, and counted as starting once asked to wake.
      row("lobby", { displayName: "Arcade", phase: "Stopped", desiredState: "Running" }),
      row("node-10", { phase: "Failed" }),
      row("node-2", { phase: "Failed" }),
    ]);
    const user = renderPage();

    expect(await tableOrder()).toEqual(["Arcade", "creative", "node-2", "node-10", "skyblock", "survival"]);

    await sortBy(user, "Sort by status");
    expect(await tableOrder()).toEqual(["skyblock", "survival", "Arcade", "node-2", "node-10", "creative"]);

    await sortBy(user, "Most players first");
    expect(await tableOrder()).toEqual(["skyblock", "survival", "Arcade", "creative", "node-2", "node-10"]);
  });

  it("leads a search with the best match and ranks equal matches by the picked order", async () => {
    calls.fleet.mockResolvedValue([
      row("my-surv", { phase: "Running", ready: true, playersOnline: 9 }),
      row("survival-b", { phase: "Running", ready: true, playersOnline: 5 }),
      row("survival-a", { phase: "Running", ready: true, playersOnline: 2 }),
    ]);
    const user = renderPage();
    await tableOrder();
    await user.type(screen.getByPlaceholderText(/Search/), "surv");

    // "surv" opens both survival names and sits inside my-surv, which ranks it below.
    expect(await tableOrder()).toEqual(["survival-a", "survival-b", "my-surv"]);
    await sortBy(user, "Most players first");
    expect(await tableOrder()).toEqual(["survival-b", "survival-a", "my-surv"]);
    await user.clear(screen.getByPlaceholderText(/Search/));
    expect(await tableOrder()).toEqual(["my-surv", "survival-b", "survival-a"]);
  });

  it("keeps servers that show the same label in name order", async () => {
    // The fleet comes off an informer cache, which lists in no fixed order.
    calls.fleet.mockResolvedValue([
      row("zeta", { displayName: "Survival" }),
      row("alpha", { displayName: "Survival" }),
    ]);
    renderPage();
    const table = await screen.findByRole("table");
    expect(within(table).getAllByText(/^(alpha|zeta)$/).map((e) => e.textContent)).toEqual(["alpha", "zeta"]);
  });

  it("shows twenty servers to a page", async () => {
    const names = Array.from({ length: 21 }, (_, i) => `srv-${String(i + 1).padStart(2, "0")}`);
    calls.fleet.mockResolvedValue(names.map((n) => row(n, {})));
    const user = renderPage();

    expect(await tableOrder()).toEqual(names.slice(0, 20));
    expect(screen.getByText("Page 1 of 2")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: /Next/ }));
    expect(await tableOrder()).toEqual(["srv-21"]);
    // A new order starts again from its first page.
    await sortBy(user, "Sort by status");
    expect(await tableOrder()).toEqual(names.slice(0, 20));
  });
});
