// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
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
