// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { AppShell } from "./AppShell";
import { PageHeader } from "./PageHeader";
import { DEFAULT_BRANDING } from "@/lib/branding";
import type { Identity, PublicBranding } from "@/lib/types";

const tier = vi.hoisted(() => ({
  identity: null as Identity | null,
  isAdmin: false,
  isOwner: false,
  loading: false,
  refresh: () => Promise.resolve(),
}));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));
const branding = vi.hoisted(() => ({ value: null as PublicBranding | null }));
vi.mock("@/lib/branding", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/branding")>();
  return { ...actual, useBranding: () => branding.value ?? actual.DEFAULT_BRANDING };
});
vi.mock("@/lib/config", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/config")>();
  return { ...actual, loadConfig: () => Promise.resolve({}) };
});

afterEach(() => {
  tier.identity = null;
  branding.value = null;
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
  it("keeps page headings and actions outside the scrolling content and replaces them on navigation", async () => {
    const action = vi.fn();
    render(<MemoryRouter><Routes><Route element={<AppShell />}>
      <Route path="/" element={<div><PageHeader title="First page" actions={<button onClick={action}>Header action</button>} /><Link to="/next">Next page</Link></div>} />
      <Route path="/next" element={<div><PageHeader title="Second page" /><p>Second content</p></div>} />
    </Route></Routes></MemoryRouter>);
    expect(screen.getByRole("main").contains(screen.getByRole("heading", { name: "First page" }))).toBe(false);
    await userEvent.click(screen.getByRole("button", { name: "Header action" }));
    expect(action).toHaveBeenCalledOnce();
    await userEvent.click(screen.getByRole("link", { name: "Next page" }));
    expect(screen.queryByRole("heading", { name: "First page" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Header action" })).toBeNull();
    expect(screen.getByRole("main").contains(screen.getByRole("heading", { name: "Second page" }))).toBe(false);
    expect(screen.getByRole("main").textContent).toContain("Second content");
  });
  // One element scrolls every page, so it would otherwise hold the last page's position.
  it("opens each page at the top of the scrolling content", async () => {
    render(<MemoryRouter><Routes><Route element={<AppShell />}>
      <Route path="/" element={<Link to="/next">Next page</Link>} />
      <Route path="/next" element={<p>Second content</p>} />
    </Route></Routes></MemoryRouter>);
    const writes: number[] = [];
    Object.defineProperty(screen.getByRole("main"), "scrollTop", { configurable: true, get: () => 0, set: (top: number) => writes.push(top) });
    await userEvent.click(screen.getByRole("link", { name: "Next page" }));
    expect(screen.getByRole("main").textContent).toContain("Second content");
    expect(writes).toEqual([0]);
  });
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

describe("AppShell branding", () => {
  it("shows the configured name and logo in the shell and the browser tab", () => {
    branding.value = { ...DEFAULT_BRANDING, name: "Acme Cloud", logo: "/api/v1/branding/logo?v=0123456789abcdef" };
    renderShell();
    expect(screen.getAllByText("Acme Cloud").length).toBeGreaterThan(0);
    const logos = screen.getAllByRole("img", { name: "Acme Cloud" });
    expect(logos.length).toBeGreaterThan(0);
    for (const logo of logos) expect(logo.getAttribute("src")).toBe("/api/v1/branding/logo?v=0123456789abcdef");
    expect(document.title).toBe("Acme Cloud · Console");
  });

  it("credits Felis in the sidebar unless the Owner turned the credit off", () => {
    const credit = () => screen.queryByRole("link", { name: i18next.t("common:powered_by") });
    renderShell();
    expect(credit()?.getAttribute("href")).toBe("https://felismc.com");
    cleanup();
    branding.value = { ...DEFAULT_BRANDING, show_powered_by: false };
    renderShell();
    expect(credit()).toBeNull();
  });
});
