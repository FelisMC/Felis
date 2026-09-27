// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { AppShell } from "./AppShell";
import type { Identity } from "@/lib/types";

const tier = vi.hoisted(() => ({
  identity: null as Identity | null,
  isAdmin: false,
  isOwner: false,
  loading: false,
  refresh: () => Promise.resolve(),
}));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
vi.mock("@/lib/config", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/config")>();
  return { ...actual, loadConfig: () => Promise.resolve({}) };
});

afterEach(() => {
  tier.identity = null;
  return i18next.changeLanguage("en-US");
});

function renderShell() {
  render(
    <MemoryRouter initialEntries={["/"]}>
      <Routes>
        <Route path="/" element={<AppShell />}>
          <Route index element={<p>page</p>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

// The line under the email in the sidebar's user card.
function roleLine(email: string) {
  return screen.getByText(email).nextElementSibling?.textContent;
}

describe("AppShell user card", () => {
  it.each([
    ["en-US", "owner", "Owner"],
    ["en-US", "user", "User"],
    ["zh-CN", "admin", "管理员"],
    ["zh-CN", "user", "普通用户"],
  ] as const)("names the role in %s: %s is %s", async (lang, role, shown) => {
    await i18next.changeLanguage(lang);
    tier.identity = { user_id: "u-1", email: "a@example.test", role } as Identity;
    renderShell();
    expect(roleLine("a@example.test")).toBe(shown);
  });

  it("names no role before it has read one", () => {
    renderShell();
    const dashes = screen.getAllByText("—");
    expect(dashes).toHaveLength(2);
    expect(screen.queryByText(/^user$/i)).toBeNull();
  });
});
