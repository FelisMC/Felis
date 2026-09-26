// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { UsersPage } from "./UsersPage";
import type { UserView } from "@/lib/types";

const calls = vi.hoisted(() => ({ listUsers: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string) => i18next.t(key);

// 45 users: three pages of 20.
function user(i: number): UserView {
  return {
    id: `u-${i}`,
    username: `player${i}`,
    role: "user",
    disabled: false,
    email_verified: true,
    server_count: 0,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

async function renderPage() {
  render(
    <MemoryRouter>
      <UsersPage />
    </MemoryRouter>,
  );
  await settle();
}

/** settle lets the mocked request resolve and the page render it. */
async function settle(ms = 0) {
  await act(() => vi.advanceTimersByTimeAsync(ms));
}

const box = () => screen.getByPlaceholderText(t("admin:users_search_placeholder"));

/** type puts text into the search box a key at a time, as typing does. (user-event
 *  stalls under fake timers, which the debounce needs.) */
function type(text: string) {
  for (let i = 1; i <= text.length; i++) fireEvent.change(box(), { target: { value: text.slice(0, i) } });
}

const next = () => fireEvent.click(screen.getByRole("button", { name: t("common:pagination_next") }));
const press = () => fireEvent.click(screen.getByRole("button", { name: t("admin:users_search_btn") }));
const newCalls = (before: number) => calls.listUsers.mock.calls.slice(before);

beforeEach(() => {
  vi.useFakeTimers();
  calls.listUsers.mockReset();
  calls.listUsers.mockImplementation(async ({ offset = 0 }: { offset?: number }) => ({
    users: Array.from({ length: Math.min(20, 45 - offset) }, (_, i) => user(offset + i)),
    total: 45,
  }));
});
afterEach(() => {
  vi.useRealTimers();
});

describe("UsersPage search", () => {
  it("searches once typing stops, from page one", async () => {
    await renderPage();
    next();
    await settle();
    expect(calls.listUsers).toHaveBeenLastCalledWith({ limit: 20, offset: 20 });
    const before = calls.listUsers.mock.calls.length;

    type("  steve ");
    await settle(299);
    expect(newCalls(before)).toEqual([]);

    await settle(1);
    expect(newCalls(before)).toEqual([[{ query: "steve", limit: 20, offset: 0 }]]);
  });

  it("applies the search at once on Enter, and only once", async () => {
    await renderPage();
    const before = calls.listUsers.mock.calls.length;

    type("alex");
    fireEvent.submit(box());
    await settle();
    expect(newCalls(before)).toEqual([[{ query: "alex", limit: 20, offset: 0 }]]);

    await settle(1000);
    expect(newCalls(before)).toHaveLength(1);
  });

  it("takes an unchanged search on page two back to page one", async () => {
    await renderPage();
    next();
    await settle();
    const before = calls.listUsers.mock.calls.length;

    press();
    await settle(1000);

    expect(newCalls(before)).toEqual([[{ limit: 20, offset: 0 }]]);
  });

  it("refreshes when the search is pressed with nothing changed on page one", async () => {
    await renderPage();
    const before = calls.listUsers.mock.calls.length;

    press();
    await settle();

    expect(newCalls(before)).toEqual([[{ limit: 20, offset: 0 }]]);
  });

  it("stays on the page someone turned to while the box is untouched", async () => {
    await renderPage();

    next();
    await settle(1000);

    expect(calls.listUsers).toHaveBeenLastCalledWith({ limit: 20, offset: 20 });
    expect(screen.getByText("player20")).toBeTruthy();
  });
});
