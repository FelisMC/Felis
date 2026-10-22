import { describe, expect, it } from "vitest";
import { droppedFiles } from "./folderUpload";
import { imageMime } from "./imagePreview";

describe("folder drops", () => {
  it("reads every browser directory batch and retains nested relative paths", async () => {
    const file = (name: string) => ({ name, isFile: true, isDirectory: false, file: (done: (file: File) => void) => done(new File([name], name)) });
    const directory = (name: string, batches: unknown[][]) => ({ name, isFile: false, isDirectory: true, createReader: () => ({ readEntries: (done: (entries: unknown[]) => void) => done(batches.shift() ?? []) }) });
    const root = directory("plugins", [[file("a.jar")], [directory("example", [[file("config.yml")]])]]);
    const data = { items: [{ kind: "file", webkitGetAsEntry: () => root }] } as unknown as DataTransfer;
    const picked = await droppedFiles(data);
    expect(picked.paths).toEqual(["plugins/a.jar", "plugins/example/config.yml"]);
    expect(picked.files.map((file) => file.name)).toEqual(["a.jar", "config.yml"]);
  });

  it("reports an unreadable directory instead of queueing a partial folder", async () => {
    const data = { items: [{ kind: "file", webkitGetAsEntry: () => ({ name: "plugins", isDirectory: true, createReader: () => ({ readEntries: (_ok: unknown, fail: (error: DOMException) => void) => fail(new DOMException("unreadable", "NotReadableError")) }) }) }] } as unknown as DataTransfer;
    await expect(droppedFiles(data)).rejects.toMatchObject({ name: "NotReadableError" });
  });
});

describe("image preview formats", () => {
  it.each([
    [new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10]), "image/png"],
    [new Uint8Array([255, 216, 255, 224]), "image/jpeg"],
    [new TextEncoder().encode("GIF89a"), "image/gif"],
    [new TextEncoder().encode("RIFF1234WEBP"), "image/webp"],
    [new TextEncoder().encode("<svg onload='test'>"), null],
    [new Uint8Array([137, 80, 78]), null],
  ])("identifies supported image bytes independently of their extension", (bytes, mime) => {
    expect(imageMime(bytes)).toBe(mime);
  });
});
