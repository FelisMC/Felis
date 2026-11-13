// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { FullFelisOnly, NanoSwitch } from "./InstallGate";

const runtime = vi.hoisted(() => ({ nano: false }));
vi.mock("@/lib/config", async (original) => ({ ...await original<typeof import("@/lib/config")>(), loadConfig: () => Promise.resolve({ apiBase: "/api/v1", rootDomain: "example.test", nano: runtime.nano }) }));

function visit(path: string) {
  render(<MemoryRouter initialEntries={[path]}><Routes>
    <Route element={<FullFelisOnly />}><Route path="/servers" element={<p>server list</p>} /></Route>
    <Route path="/account" element={<p>account page</p>} />
    <Route path="/admin/platform" element={<NanoSwitch nano={<p>nano settings</p>} full={<p>platform settings</p>} />} />
  </Routes></MemoryRouter>);
}

describe("install gate", () => {
  it("sends a Felis-nano visitor from a server page to their account", async () => {
    runtime.nano = true;
    visit("/servers");
    expect(await screen.findByText("account page")).toBeTruthy();
    expect(screen.queryByText("server list")).toBeNull();
  });

  it("leaves server pages alone on a full install", async () => {
    runtime.nano = false;
    visit("/servers");
    expect(await screen.findByText("server list")).toBeTruthy();
  });

  it("shows the nano settings page only on Felis-nano", async () => {
    runtime.nano = true;
    visit("/admin/platform");
    expect(await screen.findByText("nano settings")).toBeTruthy();
  });
});
