import { describe, it, expect } from "vitest";
import { humanizeError } from "@/lib/api";
import { opErrorText } from "./opText";

const e = (code: string, over: Record<string, unknown> = {}) => ({ code, message: "raw words", ...over });

describe("opErrorText", () => {
  it("tells an upload that found its file there from an extraction that would replace files", () => {
    expect(opErrorText("upload", e("file_exists"))).toBe(
      "A file with this name is already here, so nothing was replaced. Upload it again and choose Replace.",
    );
    expect(opErrorText("unzip", e("file_exists", { conflicts: ["a", "b"], conflict_count: 250 }))).toBe(
      "It would replace 250 files already here, so nothing was extracted yet.",
    );
    // Without the count, the list is counted.
    expect(opErrorText("unzip", e("file_exists", { conflicts: ["a"] }))).toBe(
      "It would replace 1 file already here, so nothing was extracted yet.",
    );
  });

  it("names both sizes of a volume too small, and falls back when they are missing", () => {
    expect(opErrorText("unzip", e("volume_full", { need: 3 * 1024 * 1024, avail: 1024 }))).toBe(
      "Not enough room on the world volume: 3.0 MiB needed, 1.0 KiB free. Nothing was changed.",
    );
    // A full volume: the Job leaves the zero out.
    expect(opErrorText("upload", e("volume_full", { need: 2048 }))).toBe(
      "Not enough room on the world volume: 2.0 KiB needed, 0 B free. Nothing was changed.",
    );
    expect(opErrorText("upload", e("volume_full"))).toBe(humanizeError({ status: 0, code: "volume_full", message: "raw words" }));
  });

  it("names the archive entry at fault", () => {
    expect(opErrorText("unzip", e("archive_invalid"))).toBe("The archive is damaged, or not a zip file. Nothing was changed.");
    expect(opErrorText("unzip", e("archive_invalid", { entry: "world/level.dat" }))).toBe(
      "world/level.dat in the archive is damaged (its size or checksum does not match). Nothing was changed.",
    );
    expect(opErrorText("unzip", e("archive_unsafe", { entry: "../../etc/passwd" }))).toBe(
      "../../etc/passwd in the archive would land outside this folder, or is a device file. Nothing in the archive was extracted.",
    );
    expect(opErrorText("unzip", e("archive_symlink", { entry: "world/link" }))).toBe(
      "world/link in the archive is a symbolic link. Nothing in the archive was extracted.",
    );
    expect(opErrorText("unzip", e("type_conflict", { entry: "plugins" }))).toBe(
      "plugins is a file on one side and a folder on the other, which replacing cannot resolve. Rename or delete plugins here, then extract again.",
    );
  });

  it("gives a Job that ran out of time its own words", () => {
    expect(opErrorText("unzip", e("job_failed", { message: "job failed: DeadlineExceeded" }))).toBe(
      "The background task did not finish within 2 hours and was stopped. Refresh the list to check, then try again.",
    );
    expect(opErrorText("upload", e("job_failed", { message: "job failed: BackoffLimitExceeded" }))).toBe(
      "The background task ended without saying why. Refresh the list to check, then try again.",
    );
  });

  it("tells an extraction killed for memory to split the archive", () => {
    // The message felis-api words a memory kill with.
    const oom = "the file operation ran out of memory (OOMKilled); an archive of this many files has to be split into smaller ones";
    expect(opErrorText("unzip", e("job_failed", { message: oom }))).toBe(
      "The archive holds more files than the extraction task has memory to list, so the system stopped it. The files on the server were not changed. Split it into several smaller zips and extract each one.",
    );
    expect(opErrorText("upload", e("job_failed", { message: oom }))).toBe(
      "The background task ran out of memory and the system stopped it. The files on the server were not changed. Try again.",
    );
  });

  it("words any other code as the file routes do", () => {
    expect(opErrorText("upload", e("file_changed"))).toBe(humanizeError({ status: 0, code: "file_changed", message: "raw words" }));
    expect(opErrorText("upload", e("file_changed"))).not.toBe("raw words");
  });
});
