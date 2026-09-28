// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { MySubmissionsPage } from "./MySubmissionsPage";
import { humanizeError } from "@/lib/api";
import type { SubmissionPage } from "@/lib/types";

const calls = vi.hoisted(() => ({
  listMySubmissions: vi.fn(),
  submissionLimits: vi.fn(),
  createSubmission: vi.fn(),
}));
const upload = vi.hoisted(() => ({ uploadContext: vi.fn() }));
vi.mock("@/lib/contextUpload", () => upload);
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
  upload.uploadContext.mockReset();
  calls.createSubmission.mockReset();
  calls.createSubmission.mockResolvedValue({ id: "sub-9", display_name: "Skyblock Pack", status: "pending_review" });
  calls.listMySubmissions.mockReset();
  calls.submissionLimits.mockReset();
  calls.submissionLimits.mockResolvedValue({ max_context_bytes: 99614720 });
  // A filtered page matches fewer rows; the counts still cover everything.
  calls.listMySubmissions.mockImplementation(async ({ status }: { status?: string }) =>
    status ? { ...PAGE, total: 3 } : PAGE,
  );
});
afterEach(() => {
  vi.useRealTimers();
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

  // The clock moves only when the test moves it, so the page is turned before
  // the search box's pause has run out however fast the machine is.
  it("keeps a page turned to right after the list loads", async () => {
    vi.useFakeTimers();
    render(
      <MemoryRouter>
        <MySubmissionsPage />
      </MemoryRouter>,
    );
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByText("Page 1 of 2")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(calls.listMySubmissions).toHaveBeenLastCalledWith({ limit: 10, offset: 10 });
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    expect(calls.listMySubmissions).toHaveBeenLastCalledWith({ limit: 10, offset: 10 });
    expect(screen.getByText("Page 2 of 2")).toBeTruthy();
  });

  it("refuses a file over the server's per-upload cap before sending it", async () => {
    render(
      <MemoryRouter>
        <MySubmissionsPage />
      </MemoryRouter>,
    );
    await screen.findByText("Create Above and Beyond");
    await vi.waitFor(() => expect(calls.submissionLimits).toHaveBeenCalled());
    const pack = new File(["x"], "pack.tar.gz", { type: "application/gzip" });
    Object.defineProperty(pack, "size", { value: 125829120 });
    await userEvent.click(screen.getByRole("button", { name: "Submit New Modpack" }));
    await userEvent.upload(await screen.findByLabelText(/Build Context/), pack);
    expect(await screen.findByText("The file is 120 MB; one upload may be at most 95 MB.")).toBeTruthy();
  });

  it("shows the server's cap under the drop zone", async () => {
    render(
      <MemoryRouter>
        <MySubmissionsPage />
      </MemoryRouter>,
    );
    await vi.waitFor(() => expect(calls.submissionLimits).toHaveBeenCalled());
    await userEvent.click(await screen.findByRole("button", { name: "Submit New Modpack" }));
    expect(await screen.findByText("Supports .tar.gz, up to 95 MB")).toBeTruthy();
  });

  it("shows the upload's progress and closes once it is stored", async () => {
    let finish: () => void = () => {};
    upload.uploadContext.mockImplementation(
      (_id: string, _file: File, opts: { onProgress: (sent: number, total: number, stored: number) => void }) =>
        new Promise<void>((resolve) => {
          opts.onProgress(1536, 4096, 1024);
          finish = resolve;
        }),
    );
    const pack = await openFormWith("Skyblock Pack");
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));

    const bar = await screen.findByRole("progressbar", { name: "Upload progress" });
    expect(bar.getAttribute("aria-valuenow")).toBe("37");
    expect(screen.getByText("1.5 KB of 4 KB")).toBeTruthy();
    expect(screen.getByText("37%")).toBeTruthy();
    expect(calls.createSubmission).toHaveBeenCalledWith("Skyblock Pack");
    expect(upload.uploadContext.mock.calls[0][0]).toBe("sub-9");
    expect(upload.uploadContext.mock.calls[0][1]).toBe(pack);
    expect(upload.uploadContext.mock.calls[0][2].resume).toBe(false);

    finish();
    await vi.waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("keeps a submission whose upload failed and carries it on instead of creating another", async () => {
    upload.uploadContext.mockImplementationOnce(
      async (_id: string, _file: File, opts: { onProgress: (sent: number, total: number, stored: number) => void }) => {
        opts.onProgress(2048, 4096, 2048);
        opts.onProgress(3072, 4096, 2048);
        throw { status: 0, code: "network_error", message: "the upload did not reach the API" };
      },
    );
    const pack = await openFormWith("Skyblock Pack");
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));

    expect(
      await screen.findByText(
        "Can't reach Felis: the network is down, or your Cloudflare Access sign-in expired. Reload the page to sign in again.",
      ),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "“Skyblock Pack” is saved and 2 KB of its build context is on the server. Submit again to continue from there.",
      ),
    ).toBeTruthy();
    expect((screen.getByLabelText(/Display Name/) as HTMLInputElement).disabled).toBe(true);

    upload.uploadContext.mockResolvedValueOnce(undefined);
    await userEvent.click(screen.getByRole("button", { name: "Continue upload" }));
    await vi.waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(calls.createSubmission).toHaveBeenCalledTimes(1);
    expect(upload.uploadContext).toHaveBeenCalledTimes(2);
    expect(upload.uploadContext.mock.calls[1][0]).toBe("sub-9");
    expect(upload.uploadContext.mock.calls[1][1]).toBe(pack);
    expect(upload.uploadContext.mock.calls[1][2].resume).toBe(true);

    // Stored: the next submission starts from an empty form.
    await userEvent.click(screen.getByRole("button", { name: "Submit New Modpack" }));
    const name = (await screen.findByLabelText(/Display Name/)) as HTMLInputElement;
    expect(name.value).toBe("");
    expect(name.disabled).toBe(false);
    expect(screen.queryByText(/is saved/)).toBeNull();
  });

  it("uploads a newly picked file from the start to the same submission", async () => {
    upload.uploadContext.mockRejectedValueOnce({ status: 0, code: "network_error", message: "x" });
    await openFormWith("Skyblock Pack");
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await screen.findByRole("button", { name: "Continue upload" });

    const other = new File(["\x1f\x8bother"], "other.tar.gz", { type: "application/gzip" });
    await userEvent.upload(screen.getByLabelText(/Build Context/), other);
    expect(screen.getByText("“Skyblock Pack” is saved. The file you picked uploads from the start.")).toBeTruthy();
    upload.uploadContext.mockResolvedValueOnce(undefined);
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await vi.waitFor(() => expect(upload.uploadContext).toHaveBeenCalledTimes(2));
    expect(calls.createSubmission).toHaveBeenCalledTimes(1);
    expect(upload.uploadContext.mock.calls[1][0]).toBe("sub-9");
    expect(upload.uploadContext.mock.calls[1][1]).toBe(other);
    expect(upload.uploadContext.mock.calls[1][2].resume).toBe(false);
  });

  it("Pause stops the upload without an error and offers to continue", async () => {
    upload.uploadContext.mockImplementation(
      (_id: string, _file: File, opts: { signal: AbortSignal; onProgress: (sent: number, total: number, stored: number) => void }) =>
        new Promise<void>((_resolve, reject) => {
          opts.onProgress(1536, 4096, 1024);
          opts.signal.addEventListener("abort", () => reject(new DOMException("The upload was cancelled", "AbortError")));
        }),
    );
    await openFormWith("Skyblock Pack");
    await userEvent.click(screen.getByRole("button", { name: "Submit" }));
    await screen.findByRole("progressbar", { name: "Upload progress" });
    await userEvent.click(screen.getByRole("button", { name: "Pause" }));

    expect(
      await screen.findByText(
        "“Skyblock Pack” is saved and 1 KB of its build context is on the server. Submit again to continue from there.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(screen.getByRole("button", { name: "Continue upload" })).toBeTruthy();
  });
});

describe("MySubmissionsPage keeps reading", () => {
  const row = PAGE.submissions[0];
  const waiting: SubmissionPage = {
    submissions: [{ ...row, status: "pending_review" }],
    total: 1,
    counts: { pending_review: 1, approved: 0, rejected: 0 },
  };
  const approved: SubmissionPage = {
    submissions: [{ ...row, status: "approved" }],
    total: 1,
    counts: { pending_review: 0, approved: 1, rejected: 0 },
  };
  const renderPage = () =>
    render(
      <MemoryRouter>
        <MySubmissionsPage />
      </MemoryRouter>,
    );

  it("picks up a review verdict at the slow pace", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.listMySubmissions.mockResolvedValue(waiting);
    renderPage();
    await screen.findByRole("button", { name: "Pending Review (1)" });
    const before = calls.listMySubmissions.mock.calls.length;

    calls.listMySubmissions.mockResolvedValue(approved);
    await act(() => vi.advanceTimersByTimeAsync(14_000));
    expect(calls.listMySubmissions.mock.calls.length).toBe(before);
    await act(() => vi.advanceTimersByTimeAsync(1_000));
    expect(calls.listMySubmissions.mock.calls.length).toBe(before + 1);
    expect(await screen.findByRole("button", { name: "Approved (1)" })).toBeTruthy();
  });

  it.each(["pending", "building"] as const)("rereads fast while a shown build is %s", async (build_status) => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.listMySubmissions.mockResolvedValue({ ...approved, submissions: [{ ...row, build_id: "b-1", build_status }] });
    renderPage();
    await screen.findByText("Create Above and Beyond");
    const before = calls.listMySubmissions.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(4_000));
    expect(calls.listMySubmissions.mock.calls.length).toBe(before + 1);
  });

  it("keeps the list when a later read fails, and says why", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    calls.listMySubmissions.mockResolvedValue(waiting);
    renderPage();
    await screen.findByRole("button", { name: "Pending Review (1)" });
    const outage = { status: 503, code: "unavailable", message: "list unavailable" };
    calls.listMySubmissions.mockRejectedValue(outage);
    await act(() => vi.advanceTimersByTimeAsync(15_000));
    expect((await screen.findByRole("alert")).textContent).toContain(humanizeError(outage));
    expect(screen.getByText("Create Above and Beyond")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Pending Review (1)" })).toBeTruthy();
  });
});

// openFormWith opens the submit dialog, names the pack and picks a 4 KiB file.
async function openFormWith(name: string): Promise<File> {
  render(
    <MemoryRouter>
      <MySubmissionsPage />
    </MemoryRouter>,
  );
  await screen.findByText("Create Above and Beyond");
  await vi.waitFor(() => expect(calls.submissionLimits).toHaveBeenCalled());
  await userEvent.click(screen.getByRole("button", { name: "Submit New Modpack" }));
  await userEvent.type(await screen.findByLabelText(/Display Name/), name);
  const pack = new File(["\x1f\x8bpack"], "pack.tar.gz", { type: "application/gzip" });
  Object.defineProperty(pack, "size", { value: 4096 });
  await userEvent.upload(screen.getByLabelText(/Build Context/), pack);
  return pack;
}
