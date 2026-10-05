import { describe, it, expect } from "vitest";
import { humanizeError } from "@/lib/api";
import { opErrorText } from "./opText";

const e = (code: string, over: Record<string, unknown> = {}) => ({ code, message: "raw words", ...over });

describe("opErrorText", () => {
  it("tells an upload that found its file there from an extraction that would replace files", () => {
    expect(opErrorText("upload", e("file_exists"))).toBe(
      "A file with this name already exists. The upload did not replace it. Select Replace when uploading again.",
    );
    expect(opErrorText("unzip", e("file_exists", { conflicts: ["a", "b"], conflict_count: 250 }))).toBe(
      "Extraction would overwrite 250 existing files. The operation has not been performed.",
    );
    // Without the count, the list is counted.
    expect(opErrorText("unzip", e("file_exists", { conflicts: ["a"] }))).toBe(
      "Extraction would overwrite 1 existing file. The operation has not been performed.",
    );
  });

  it("names both sizes of a volume too small, and falls back when they are missing", () => {
    expect(opErrorText("unzip", e("volume_full", { need: 3 * 1024 * 1024, avail: 1024 }))).toBe(
      "Insufficient world volume space: 3.0 MiB required, 1.0 KiB available. This operation did not modify files.",
    );
    // A full volume: the Job leaves the zero out.
    expect(opErrorText("upload", e("volume_full", { need: 2048 }))).toBe(
      "Insufficient world volume space: 2.0 KiB required, 0 B available. This operation did not modify files.",
    );
    expect(opErrorText("upload", e("volume_full"))).toBe(humanizeError({ status: 0, code: "volume_full", message: "raw words" }));
  });

  it("names the archive entry at fault", () => {
    expect(opErrorText("unzip", e("archive_invalid"))).toBe("The archive is damaged or is not in ZIP format. This operation did not modify files.");
    expect(opErrorText("unzip", e("archive_invalid", { entry: "world/level.dat" }))).toBe(
      "The size or checksum of archive entry world/level.dat does not match. This operation did not modify files.",
    );
    expect(opErrorText("unzip", e("archive_unsafe", { entry: "../../etc/passwd" }))).toBe(
      "Archive entry ../../etc/passwd contains an out-of-directory path or a device file. The entire archive was rejected without extraction.",
    );
    expect(opErrorText("unzip", e("archive_symlink", { entry: "world/link" }))).toBe(
      "Archive entry world/link is a symbolic link. The entire archive was rejected without extraction.",
    );
    expect(opErrorText("unzip", e("type_conflict", { entry: "plugins" }))).toBe(
      "Entry plugins conflicts with an existing entry of the same name because one is a file and the other is a folder. Rename or delete the existing plugins before extraction.",
    );
  });

  it("gives a Job that ran out of time its own words", () => {
    expect(opErrorText("unzip", e("job_failed", { message: "job failed: DeadlineExceeded" }))).toBe(
      "The background task exceeded its two-hour execution limit and was terminated. Refresh the file list to verify the result before retrying.",
    );
    expect(opErrorText("upload", e("job_failed", { message: "job failed: BackoffLimitExceeded" }))).toBe(
      "The background task ended without a failure reason. Refresh the file list to verify the result before retrying.",
    );
  });

  it("tells an extraction killed for memory to split the archive", () => {
    // The message felis-api words a memory kill with.
    const oom = "the file operation ran out of memory (OOMKilled); an archive of this many files has to be split into smaller ones";
    expect(opErrorText("unzip", e("job_failed", { message: oom }))).toBe(
      "The extraction task terminated because its file list exceeded available memory. Server files were not modified. Split the content into smaller ZIP archives and upload and extract them separately.",
    );
    expect(opErrorText("upload", e("job_failed", { message: oom }))).toBe(
      "The background task terminated due to insufficient memory. Server files were not modified. Check task resources before retrying.",
    );
  });

  it("words any other code as the file routes do", () => {
    expect(opErrorText("upload", e("file_changed"))).toBe(humanizeError({ status: 0, code: "file_changed", message: "raw words" }));
    expect(opErrorText("upload", e("file_changed"))).not.toBe("raw words");
  });
});
