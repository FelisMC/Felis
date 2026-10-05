// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { AppShell } from "./AppShell";
import { PageHeader } from "./PageHeader";
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
