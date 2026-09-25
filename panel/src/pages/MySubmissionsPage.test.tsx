// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { MySubmissionsPage } from "./MySubmissionsPage";
import type { SubmissionPage } from "@/lib/types";

const calls = vi.hoisted(() => ({
  listMySubmissions: vi.fn(),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

// One row of this player's 14 submissions.
const PAGE: SubmissionPage = {
  submissions: [
    {
      id: "sub-3",
      submitted_by: "user-1",
      display_name: "Create Above and Beyond",
      context_ref: "submissions/sub-3.tar.gz",
      status: "approved",
      created_at: new Date().toISOString(),
    },
  ],
  total: 14,
  counts: { pending_review: 2, approved: 9, rejected: 3 },
};

beforeEach(() => {
  calls.listMySubmissions.mockReset();
  // A filtered page matches fewer rows; the counts still cover everything.
  calls.listMySubmissions.mockImplementation(async ({ status }: { status?: string }) =>
    status ? { ...PAGE, total: 3 } : PAGE,
  );
});
afterEach(() => {
  vi.restoreAllMocks();
  return i18next.changeLanguage("en-US");
});

describe("MySubmissionsPage", () => {
  it("counts every submission from the server and asks it for the filtered page", async () => {
    render(
      <MemoryRouter>
        <MySubmissionsPage />
      </MemoryRouter>,
    );
    await screen.findByText("Create Above and Beyond");
    expect(screen.getByRole("button", { name: "All Statuses (14)" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Rejected (3)" })).toBeTruthy();
    expect(screen.getByText("Page 1 of 2")).toBeTruthy();

    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await vi.waitFor(() => expect(calls.listMySubmissions).toHaveBeenLastCalledWith({ limit: 10, offset: 10 }));
    await userEvent.click(screen.getByRole("button", { name: "Rejected (3)" }));
    await userEvent.type(screen.getByPlaceholderText("Search submissions..."), "create");
    await vi.waitFor(() =>
      expect(calls.listMySubmissions).toHaveBeenLastCalledWith({ status: "rejected", query: "create", limit: 10, offset: 0 }),
    );
    expect(screen.getByRole("button", { name: "All Statuses (14)" })).toBeTruthy();
    await vi.waitFor(() => expect(screen.queryByText(/^Page \d+ of/)).toBeNull());
  });
});
