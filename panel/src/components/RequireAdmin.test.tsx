// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { humanizeError } from "@/lib/api";
import { RequireAdmin } from "./RequireAdmin";
import { RequireOwner } from "./RequireOwner";

const tier = vi.hoisted(() => ({
  loading: false,
  isAdmin: false,
  isOwner: false,
  identityError: null as unknown,
  revalidate: vi.fn(),
}));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));

const t = (key: string) => i18next.t(key);
const err503 = { status: 503, code: "unavailable", message: "database is restarting" };

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route element={<RequireAdmin />}>
          <Route path="/admin" element={<p>admin page</p>} />
        </Route>
        <Route element={<RequireOwner />}>
          <Route path="/admin/users" element={<p>users page</p>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

const retryButton = () => screen.getByRole("button", { name: t("common:try_again") });

beforeEach(() => {
  Object.assign(tier, { loading: false, isAdmin: false, isOwner: false, identityError: null });
  tier.revalidate.mockReset();
  tier.revalidate.mockResolvedValue(undefined);
});

describe.each([
  ["RequireAdmin", "/admin", "admin page", "isAdmin"],
  ["RequireOwner", "/admin/users", "users page", "isOwner"],
] as const)("%s", (_name, path, page, flag) => {
  it("says the access check failed, with why, when /me failed with anything but a 401", () => {
    tier.identityError = err503;
    renderAt(path);

    expect(screen.getByText(t("common:access_unknown_title"))).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toBe(humanizeError(err503));
    expect(retryButton()).toBeTruthy();
    expect(screen.queryByText(t("common:not_authorized_title"))).toBeNull();
    expect(screen.queryByText(page)).toBeNull();
  });

  it("re-reads /me in place from the retry, and holds the button until it answers", async () => {
    tier.identityError = err503;
    let answer!: () => void;
    tier.revalidate.mockReturnValue(new Promise<void>((resolve) => (answer = resolve)));
    renderAt(path);

    fireEvent.click(retryButton());
    expect(tier.revalidate).toHaveBeenCalledTimes(1);
    expect((retryButton() as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(retryButton());
    expect(tier.revalidate).toHaveBeenCalledTimes(1);

    await act(async () => answer());
    expect((retryButton() as HTMLButtonElement).disabled).toBe(false);
  });

  it("still says not authorized to a known account without the role", () => {
    renderAt(path);

    expect(screen.getByText(t("common:not_authorized_title"))).toBeTruthy();
    expect(screen.queryByText(t("common:access_unknown_title"))).toBeNull();
  });

  it("renders the page for an account with the role", () => {
    tier[flag] = true;
    renderAt(path);

    expect(screen.getByText(page)).toBeTruthy();
  });
});
