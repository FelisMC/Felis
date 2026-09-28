// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import i18next from "i18next";
import type { FileOp } from "@/lib/types";
import { humanizeError } from "@/lib/api";
import { FileOps } from "./FileOps";
import { opErrorText } from "./opText";

const t = (key: string, opts?: Record<string, unknown>) => i18next.t(`files:${key}`, opts ?? {});

function op(over: Partial<FileOp> = {}): FileOp {
  return { id: "a", op: "unzip", path: "pack.zip", state: "running", started_at: "2026-09-28T00:00:00Z", done: 0, total: 0, ...over };
}

function show(ops: FileOp[], error: unknown = null) {
  const onDismiss = vi.fn();
  const onConflicts = vi.fn();
  const view = render(<FileOps ops={ops} error={error} onDismiss={onDismiss} onConflicts={onConflicts} />);
  return { ...view, onDismiss, onConflicts };
}

describe("FileOps", () => {
  it("shows nothing with no ops and nothing to say", () => {
    const { container } = show([]);
    expect(container.innerHTML).toBe("");
  });

  it("counts a running op in whole percents, rounding down and never past 100", () => {
    show([op({ id: "a", path: "a.zip", done: 999, total: 1000 }), op({ id: "b", path: "b.zip", done: 5, total: 4 })]);

    expect(screen.getByText(t("op_progress", { done: "999 B", total: "1000 B", percent: 99 }))).toBeTruthy();
    expect(screen.getByText(t("op_progress", { done: "5 B", total: "4 B", percent: 100 }))).toBeTruthy();
    const bars = screen.getAllByRole("progressbar");
    expect(bars.map((b) => [b.getAttribute("aria-label"), b.getAttribute("aria-valuenow")])).toEqual([
      [t("op_progress_label", { path: "a.zip" }), "99"],
      [t("op_progress_label", { path: "b.zip" }), "100"],
    ]);
  });

  it("claims no progress before the Job has counted, and offers no dismissal while it runs", () => {
    show([op()]);

    expect(screen.getByText(t("op_preparing"))).toBeTruthy();
    expect(screen.getByRole("progressbar").hasAttribute("aria-valuenow")).toBe(false);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("words how each kind of op ended", () => {
    show([
      op({ id: "u", op: "upload", path: "big.jar", state: "succeeded" }),
      op({ id: "z", path: "pack.zip", state: "succeeded", files: 1, bytes: 2048 }),
      op({ id: "f", op: "upload", path: "lost.jar", state: "failed" }),
    ]);

    expect(screen.getByText(t("op_upload", { path: "big.jar" }))).toBeTruthy();
    expect(screen.getByText(t("op_upload_done"))).toBeTruthy();
    expect(screen.getByText(t("op_unzip", { path: "pack.zip" }))).toBeTruthy();
    expect(screen.getByText(t("op_unzip_done", { count: 1, bytes: "2.0 KiB" }))).toBeTruthy();
    // A failure the Job left unexplained still says something.
    expect(screen.getByText(opErrorText("upload", { code: "job_failed", message: "" }))).toBeTruthy();
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  it("offers the conflicts of an extraction only, and dismisses each ended op by its id", () => {
    const exists = { code: "file_exists", message: "", conflicts: ["a.txt"] };
    const refused = op({ id: "z", path: "pack.zip", state: "failed", error: exists });
    const { onDismiss, onConflicts } = show([
      refused,
      op({ id: "u", op: "upload", path: "a.txt", state: "failed", error: exists }),
      op({ id: "bad", path: "bad.zip", state: "failed", error: { code: "archive_invalid", message: "" } }),
    ]);

    const conflicts = screen.getAllByRole("button", { name: t("op_conflicts") });
    expect(conflicts).toHaveLength(1);
    fireEvent.click(conflicts[0]);
    expect(onConflicts.mock.calls).toEqual([[refused]]);

    fireEvent.click(screen.getByRole("button", { name: t("op_dismiss", { path: "a.txt" }) }));
    expect(onDismiss.mock.calls).toEqual([["u"]]);
  });

  it("says why the ops could not be read, over the ones it has", () => {
    const broke = { status: 500, code: "internal", message: "" };
    show([op({ state: "succeeded", files: 1, bytes: 8 })], broke);

    const region = screen.getByRole("region", { name: t("ops_label") });
    expect(within(region).getByText(t("ops_refresh_failed", { reason: humanizeError(broke) }))).toBeTruthy();
    expect(within(region).getByText(t("op_unzip_done", { count: 1, bytes: "8 B" }))).toBeTruthy();
  });
});
