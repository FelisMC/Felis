// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { FleetServer } from "@/lib/types";
import { ServersPage } from "./ServersPage";

const tier = vi.hoisted(() => ({ isAdmin: true, identity: { email: "admin@example.test" } }));
const calls = vi.hoisted(() => ({ fleet: vi.fn(), myServers: vi.fn() }));

vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
vi.mock("@/lib/config", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/config")>();
  return {
    ...actual,
    loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "example.test" }),
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
