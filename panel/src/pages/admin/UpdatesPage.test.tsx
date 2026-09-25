// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { UpdatesPage } from "./UpdatesPage";

const calls = vi.hoisted(() => ({
  getUpdateWindow: vi.fn(),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

beforeEach(() => {
  calls.getUpdateWindow.mockReset();
  calls.getUpdateWindow.mockResolvedValue({ start: null, end: null });
});
afterEach(() => {
  vi.restoreAllMocks();
  return i18next.changeLanguage("en-US");
});

describe("UpdatesPage", () => {
  it("says the window is advisory, since nothing applies an update on its own", async () => {
    render(
      <MemoryRouter>
        <UpdatesPage />
      </MemoryRouter>,
    );
    expect(
      await screen.findByText(
        "The window is advisory. Updates happen only when someone runs the apply commands `felis update` prints; it reads this window and warns when run outside it.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("No maintenance window set. `felis update` will say so and leave the timing to you.")).toBeTruthy();
    expect(screen.queryByText(/applied automatically|may apply a Scheduled update/)).toBeNull();
  });
});
