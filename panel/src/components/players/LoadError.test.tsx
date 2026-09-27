// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { ReactElement } from "react";
import i18next from "i18next";
import { humanizeError } from "@/lib/api";
import { OnlineSection } from "./OnlineSection";
import { WhitelistSection } from "./WhitelistSection";
import { BansSection } from "./BansSection";
import { WakeListSection } from "./WakeListSection";

const calls = vi.hoisted(() => ({
  accessPlayers: vi.fn(),
  accessWhitelistList: vi.fn(),
  accessBanList: vi.fn(),
  serverAllowlist: vi.fn(),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
});

// A lapsed session and a server mid-restart want different next steps, so a
// section whose list failed says which one it hit.
describe("a player list that failed to load", () => {
  it.each<[string, keyof typeof calls, string, ReactElement]>([
    ["online players", "accessPlayers", "servers:access_online_load_error", <OnlineSection name="lobby" />],
    ["whitelist", "accessWhitelistList", "servers:access_whitelist_load_error", <WhitelistSection name="lobby" />],
    ["ban list", "accessBanList", "servers:access_ban_load_error", <BansSection name="lobby" />],
    ["wake list", "serverAllowlist", "servers:wake_list_load_error", <WakeListSection name="lobby" policy="allowlist" />],
  ])("names why the %s did not load", async (_label, call, key, section) => {
    const outage = { status: 409, code: "not_running", message: "server is not running" };
    calls[call].mockRejectedValue(outage);
    render(section);

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(`${i18next.t(key)} ${humanizeError(outage)}`);
    expect(humanizeError(outage)).not.toBe("");
  });
});
