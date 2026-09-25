// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { ErrorBoundary } from "./ErrorBoundary";

const { reloadForNewDeploy } = vi.hoisted(() => ({ reloadForNewDeploy: vi.fn() }));
vi.mock("@/lib/chunk", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/chunk")>();
  return { ...actual, reloadForNewDeploy };
});

const t = (key: string) => i18next.t(key);

const state = { broken: true, error: new Error("boom") as unknown };
function Page() {
  if (state.broken) throw state.error;
  return <p>page content</p>;
}

// React and the boundary both log the caught render error, and React's dev
// build replays it as a window error event that jsdom would print too.
let quiet: ReturnType<typeof vi.spyOn>;
const swallow = (e: ErrorEvent) => e.preventDefault();
beforeEach(() => {
  state.broken = true;
  state.error = new Error("boom");
  reloadForNewDeploy.mockReset();
  quiet = vi.spyOn(console, "error").mockImplementation(() => {});
  window.addEventListener("error", swallow);
});
afterEach(() => {
  quiet.mockRestore();
  window.removeEventListener("error", swallow);
});

describe("ErrorBoundary", () => {
  it("shows the crash page with the error instead of a blank screen", () => {
    render(
      <ErrorBoundary>
        <Page />
      </ErrorBoundary>,
    );

    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText(t("common:crash_title"))).toBeTruthy();
    expect(screen.getByText("Error: boom")).toBeTruthy();
    expect(reloadForNewDeploy).not.toHaveBeenCalled();
  });

  it("try again renders the page once it stops throwing", async () => {
    render(
      <ErrorBoundary>
        <Page />
      </ErrorBoundary>,
    );

    state.broken = false;
    await userEvent.click(screen.getByRole("button", { name: t("common:try_again") }));

    expect(screen.getByText("page content")).toBeTruthy();
  });

  it("clears the crash when the route changes", () => {
    const view = render(
      <ErrorBoundary resetKey="/servers/a">
        <Page />
      </ErrorBoundary>,
    );
    expect(screen.getByRole("alert")).toBeTruthy();

    state.broken = false;
    view.rerender(
      <ErrorBoundary resetKey="/servers/b">
        <Page />
      </ErrorBoundary>,
    );

    expect(screen.getByText("page content")).toBeTruthy();
  });

  it("uses the caller's fallback when given one", () => {
    render(
      <ErrorBoundary fallback={() => <p>flat fleet</p>}>
        <Page />
      </ErrorBoundary>,
    );

    expect(screen.getByText("flat fleet")).toBeTruthy();
  });

  it("reloads once for a chunk from an older deploy and says the panel updated", () => {
    state.error = new TypeError("Failed to fetch dynamically imported module: /assets/Files-abc.js");
    render(
      <ErrorBoundary>
        <Page />
      </ErrorBoundary>,
    );

    expect(reloadForNewDeploy).toHaveBeenCalledOnce();
    expect(screen.getByText(t("common:crash_updated_title"))).toBeTruthy();
  });
});
