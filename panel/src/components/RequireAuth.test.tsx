// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import i18next from "i18next";
import { RequireAuth } from "./RequireAuth";

const tier = vi.hoisted(() => ({ loading: false, unauthenticated: false, sessionEnded: false }));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));

function LoginProbe() {
  const location = useLocation();
  return (
    <p>
      login{location.search} ended={String((location.state as { sessionEnded?: boolean } | null)?.sessionEnded ?? false)}
    </p>
  );
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/login" element={<LoginProbe />} />
        <Route element={<RequireAuth />}>
          <Route path="/" element={<p>dashboard</p>} />
          <Route path="/servers/:name" element={<p>console</p>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  Object.assign(tier, { loading: false, unauthenticated: false, sessionEnded: false });
});

describe("RequireAuth", () => {
  it("shows a spinner while /me is in flight, never the login page", () => {
    tier.loading = true;
    tier.unauthenticated = true;
    renderAt("/servers/lobby");

    expect(screen.getByText(i18next.t("common:loading"))).toBeTruthy();
    expect(screen.queryByText(/^login/)).toBeNull();
  });

  it("sends a 401 to /login with the page to come back to", () => {
    tier.unauthenticated = true;
    renderAt("/servers/lobby?tab=files");

    expect(screen.getByText(`login?next=${encodeURIComponent("/servers/lobby?tab=files")} ended=false`)).toBeTruthy();
  });

  it("drops ?next= for the dashboard and says when the session ended", () => {
    tier.unauthenticated = true;
    tier.sessionEnded = true;
    renderAt("/");

    expect(screen.getByText("login ended=true")).toBeTruthy();
  });

  it("renders the app for a session, including a degraded /me", () => {
    renderAt("/servers/lobby");

    expect(screen.getByText("console")).toBeTruthy();
  });
});
