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
  it("explains the maintenance guard and explicit apply flow", async () => {
    render(
      <MemoryRouter>
        <UpdatesPage />
      </MemoryRouter>,
    );
    expect(
      await screen.findByText(
        "Apply is allowed inside this window. The host checks it before taking a backup and again before installation. Outside the window, apply is refused unless `--now` explicitly starts manual maintenance; `--force` does not bypass the checks.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("No maintenance window set. Configure one before applying, or explicitly use `--apply --now` for manual maintenance.")).toBeTruthy();
    expect(screen.queryByText(/applied automatically|may apply a Scheduled update/)).toBeNull();
  });
});
