// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { SubmissionsPage } from "./SubmissionsPage";
import type { Submission, SubmissionPage } from "@/lib/types";

const calls = vi.hoisted(() => ({
  listSubmissions: vi.fn(),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const SUB: Submission = {
  id: "sub-7",
  submitted_by: "player@example.test",
  display_name: "Sky Block Pack",
  context_ref: "submissions/sub-7.tar.gz",
  status: "pending_review",
  created_at: new Date().toISOString(),
  context_sha256: "c".repeat(64),
};

// One row of a 23-row queue: the rest live on the server's other pages.
const PAGE: SubmissionPage = {
  submissions: [SUB],
  total: 23,
  counts: { pending_review: 5, approved: 17, rejected: 1 },
};

beforeEach(() => {
  calls.listSubmissions.mockReset();
  calls.listSubmissions.mockResolvedValue(PAGE);
});
afterEach(() => {
  vi.restoreAllMocks();
  return i18next.changeLanguage("en-US");
});

function renderPage() {
  return render(
    <MemoryRouter>
      <SubmissionsPage />
    </MemoryRouter>,
  );
}

describe("SubmissionsPage", () => {
  it("counts the whole queue from the server's counts, whatever one page holds", async () => {
    renderPage();
    await screen.findByText("Sky Block Pack");
    expect(screen.getByRole("button", { name: "All (23)" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Pending Review (5)" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Approved (17)" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Rejected (1)" })).toBeTruthy();
    expect(screen.getByText("Page 1 of 3")).toBeTruthy();
  });

  it("asks the server for each page and filter, and a new filter or search starts at page one", async () => {
    renderPage();
    await screen.findByText("Sky Block Pack");
    expect(calls.listSubmissions).toHaveBeenLastCalledWith({ limit: 10, offset: 0 });

    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await vi.waitFor(() => expect(calls.listSubmissions).toHaveBeenLastCalledWith({ limit: 10, offset: 10 }));

    await userEvent.click(screen.getByRole("button", { name: "Approved (17)" }));
    await vi.waitFor(() =>
      expect(calls.listSubmissions).toHaveBeenLastCalledWith({ status: "approved", limit: 10, offset: 0 }),
    );

    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await vi.waitFor(() =>
      expect(calls.listSubmissions).toHaveBeenLastCalledWith({ status: "approved", limit: 10, offset: 10 }),
    );
    await userEvent.type(screen.getByRole("textbox"), "  sky ");
    await vi.waitFor(() =>
      expect(calls.listSubmissions).toHaveBeenLastCalledWith({ status: "approved", query: "sky", limit: 10, offset: 0 }),
    );
  });

  it("steps back to the last page that still has rows when its own page comes back empty", async () => {
    // Reviews elsewhere shrank the queue to 20 rows: page 3 no longer exists.
    calls.listSubmissions.mockImplementation(async ({ offset }: { offset: number }) =>
      offset === 20 ? { ...PAGE, submissions: [], total: 20 } : PAGE,
    );
    renderPage();
    await screen.findByText("Sky Block Pack");
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await screen.findByText("Page 2 of 3");
    await userEvent.click(screen.getByRole("button", { name: "Next" }));
    await vi.waitFor(() => expect(calls.listSubmissions.mock.calls.map(([o]) => o.offset)).toEqual([0, 10, 20, 10]));
    expect(await screen.findByText("Sky Block Pack")).toBeTruthy();
  });

  it("says nothing matched when a filter empties the page, and hides the pager for one page", async () => {
    renderPage();
    await screen.findByText("Sky Block Pack");
    calls.listSubmissions.mockResolvedValue({ ...PAGE, submissions: [], total: 0 });
    await userEvent.click(screen.getByRole("button", { name: "Rejected (1)" }));
    expect(await screen.findByText("No matches")).toBeTruthy();
    expect(screen.queryByText(/^Page \d+ of/)).toBeNull();
  });
});
