// @vitest-environment jsdom
import { createHash } from "node:crypto";
import { StrictMode } from "react";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { ServerFiles } from "./ServerFiles";
import { humanizeError } from "@/lib/api";
import { ONE_REQUEST_BYTES } from "@/components/files/useUploads";
import { ToastProvider } from "@/components/Toaster";
import { OP_POLL_MS } from "@/components/files/sessionUpload";
import { EXPORT_POLL_MS } from "@/lib/download";
import { STATUS_POLL_FAST_MS } from "@/lib/hooks";
import type { FileOp, ServerJob } from "@/lib/types";

const mocks = vi.hoisted(() => ({
  writeServerFile: vi.fn(),
  status: vi.fn(),
  stop: vi.fn(),
  listServerFiles: vi.fn(),
  readServerFile: vi.fn(),
  createServerFile: vi.fn(),
  deleteServerFile: vi.fn(),
  mkdirServerFolder: vi.fn(),
  renameServerFile: vi.fn(),
  uploadServerFile: vi.fn(),
  listServerFileOps: vi.fn(),
  unzipServerFile: vi.fn(),
  downloadServerFile: vi.fn(),
  exportStatus: vi.fn(),
  exportDownloadURL: vi.fn(),
  beginServerFileUpload: vi.fn(),
  getServerFileUpload: vi.fn(),
  putServerFileUploadPart: vi.fn(),
  deleteServerFileUpload: vi.fn(),
  commitServerFileUpload: vi.fn(),
  serverJobs: vi.fn(),
  searchServerFiles: vi.fn(),
  transferServerFiles: vi.fn(),
  zipServerFiles: vi.fn(),
  fetchServerFile: vi.fn(),
  downloadServerFiles: vi.fn(),
  previewServerFile: vi.fn(),
  serverPaperGlobal: vi.fn(),
  patchServerPaperGlobal: vi.fn(),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      status: mocks.status,
      stop: mocks.stop,
      listServerFiles: mocks.listServerFiles,
      readServerFile: mocks.readServerFile,
      writeServerFile: mocks.writeServerFile,
      createServerFile: mocks.createServerFile,
      deleteServerFile: mocks.deleteServerFile,
      mkdirServerFolder: mocks.mkdirServerFolder,
      renameServerFile: mocks.renameServerFile,
      uploadServerFile: mocks.uploadServerFile,
      listServerFileOps: mocks.listServerFileOps,
      unzipServerFile: mocks.unzipServerFile,
      downloadServerFile: mocks.downloadServerFile,
      exportStatus: mocks.exportStatus,
      exportDownloadURL: mocks.exportDownloadURL,
      beginServerFileUpload: mocks.beginServerFileUpload,
      getServerFileUpload: mocks.getServerFileUpload,
      putServerFileUploadPart: mocks.putServerFileUploadPart,
      deleteServerFileUpload: mocks.deleteServerFileUpload,
      commitServerFileUpload: mocks.commitServerFileUpload,
      serverJobs: mocks.serverJobs,
      searchServerFiles: mocks.searchServerFiles,
      transferServerFiles: mocks.transferServerFiles,
      zipServerFiles: mocks.zipServerFiles,
      fetchServerFile: mocks.fetchServerFile,
      downloadServerFiles: mocks.downloadServerFiles,
      previewServerFile: mocks.previewServerFile,
      serverPaperGlobal: mocks.serverPaperGlobal,
      patchServerPaperGlobal: mocks.patchServerPaperGlobal,
    },
  };
});
vi.mock("@/lib/tier", () => ({ useTier: () => ({ isAdmin: true, loading: false }) }));
// Editor interactions are exercised in the browser; page tests focus on reads,
// writes, conflict protection and the dialog lifecycle.
vi.mock("@/components/files/TextFileEditor", () => ({
  TextFileEditor: ({ value, onChange, label }: { value: string; onChange: (v: string) => void; label: string }) =>
    <textarea aria-label={label} value={value} onChange={(e) => onChange(e.target.value)} />,
}));

const t = (key: string) => i18next.t(key);

function renderFiles(strict = false) {
  return render(
    <ToastProvider>
      <MemoryRouter initialEntries={["/servers/lobby/files"]}>
        <Routes>
          <Route path="/servers/:name/files" element={<ServerFiles />} />
        </Routes>
      </MemoryRouter>
    </ToastProvider>,
    { wrapper: strict ? StrictMode : undefined },
  );
}

async function openEditor() {
  renderFiles();
  await userEvent.click(await screen.findByText("server.properties"));
  const dialog = await screen.findByRole("dialog");
  await userEvent.click(await within(dialog).findByRole("radio", { name: t("files:properties_text") }));
  return { dialog, editor: await within(dialog).findByRole("textbox", { name: t("files:file_content") }) as HTMLTextAreaElement };
}

let opsNow: FileOp[] = [];
// The server's backup, restore and export Jobs, as each read finds them.
let jobsNow: ServerJob[] = [];
const fileOp = (over: Partial<FileOp> = {}): FileOp => ({
  id: "op1",
  op: "unzip",
  path: "pack.zip",
  state: "running",
  started_at: "2026-09-28T00:00:00Z",
  done: 0,
  total: 0,
  ...over,
});

const stopped = { name: "lobby", subdomain: "lobby", phase: "Stopped", desiredState: "Stopped", ready: false };

beforeEach(() => {
  mocks.serverPaperGlobal.mockReset().mockResolvedValue({ values: { "watchdog.early-warning-delay": 10000 }, sha256: "a".repeat(64) });
  mocks.patchServerPaperGlobal.mockReset().mockResolvedValue({ values: { "watchdog.early-warning-delay": 12000 }, sha256: "b".repeat(64) });
  mocks.writeServerFile.mockReset();
  mocks.listServerFiles.mockReset();
  mocks.listServerFiles.mockImplementation((_name: string, path: string) =>
    Promise.resolve({
      path,
      truncated: false,
      entries: [
        { name: "server.properties", size: 8, is_dir: false, mod_time: "2026-09-01T00:00:00Z" },
        { name: "world", size: 0, is_dir: true, mod_time: "2026-09-01T00:00:00Z" },
      ],
    }),
  );
  mocks.readServerFile.mockReset();
  mocks.readServerFile.mockImplementation((_name: string, path: string) =>
    Promise.resolve({ path, content: btoa("motd=hi\n"), sha256: "abc" }),
  );
  for (const m of [mocks.createServerFile, mocks.deleteServerFile, mocks.mkdirServerFolder, mocks.renameServerFile, mocks.uploadServerFile]) {
    m.mockReset();
  }
  for (const m of [
    mocks.unzipServerFile,
    mocks.downloadServerFile,
    mocks.exportStatus,
    mocks.exportDownloadURL,
    mocks.beginServerFileUpload,
    mocks.getServerFileUpload,
    mocks.putServerFileUploadPart,
    mocks.deleteServerFileUpload,
    mocks.commitServerFileUpload,
    mocks.searchServerFiles,
    mocks.transferServerFiles,
    mocks.zipServerFiles,
    mocks.fetchServerFile,
    mocks.downloadServerFiles,
    mocks.previewServerFile,
  ]) {
    m.mockReset();
  }
  // The server's background ops, as each read finds them.
  opsNow = [];
  mocks.listServerFileOps.mockReset();
  mocks.listServerFileOps.mockImplementation(async () => ({ ops: opsNow }));
  jobsNow = [];
  mocks.serverJobs.mockReset();
  mocks.serverJobs.mockImplementation(async () => jobsNow);
  localStorage.clear();
  mocks.stop.mockReset();
  mocks.status.mockReset();
  mocks.status.mockResolvedValue(stopped);
});
afterEach(() => {
  vi.useRealTimers();
});

describe("file manager actions", () => {
  const label = (key: string, name: string) => i18next.t(`files:${key}`, { name });

  it("keeps file downloads and confirmed deletion available when the quota is full", async () => {
    mocks.status.mockResolvedValue({ ...stopped, diskFull: true, disk: { usedBytes: 1200, limitBytes: 1000, sampledAt: "2026-10-24T00:00:00Z", stale: true } });
    mocks.deleteServerFile.mockResolvedValue(undefined);
    renderFiles();
    await screen.findByText("server.properties");
    expect(screen.getByRole("meter", { name: t("servers:disk_title") })).toBeTruthy();
    expect(screen.getByText(t("files:disk_cleanup_hint"))).toBeTruthy();
    expect(screen.getByRole("button", { name: label("download_item", "server.properties") })).toHaveProperty("disabled", false);
    await userEvent.click(screen.getByRole("button", { name: label("delete_item", "world") }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:delete") }));
    expect(mocks.deleteServerFile).toHaveBeenCalledExactlyOnceWith("lobby", "world");
  });

  it("searches on submit and opens nested results using their full relative path", async () => {
    mocks.searchServerFiles.mockResolvedValue({ path: "", entries: [{ name: "plugins/example/config.yml", size: 12, is_dir: false, mod_time: "" }], truncated: true });
    renderFiles();
    await screen.findByText("world");
    await userEvent.type(screen.getByRole("textbox", { name: t("files:search_label") }), "config");
    expect(mocks.searchServerFiles).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: t("files:search") }));
    expect(mocks.searchServerFiles).toHaveBeenCalledWith("lobby", "", "config");
    await userEvent.click(await screen.findByText("plugins/example/config.yml"));
    expect(mocks.readServerFile).toHaveBeenCalledWith("lobby", "plugins/example/config.yml");
    expect(screen.getByText(t("files:search_truncated"))).toBeTruthy();
  });

  it("confirms copy conflicts and retries the same sources and destination with overwrite", async () => {
    mocks.transferServerFiles.mockResolvedValueOnce({ op: fileOp({ op: "copy", path: "world", state: "failed", error: { code: "file_exists", message: "exists", conflicts: ["world/server.properties"], conflict_count: 1 } }) })
      .mockResolvedValueOnce({ op: fileOp({ id: "copy2", op: "copy", path: "world", state: "succeeded" }) });
    renderFiles();
    await userEvent.click(await screen.findByRole("checkbox", { name: label("select_item", "server.properties") }));
    // Guarded files such as server.properties cannot be copied.
    expect((screen.getByRole("button", { name: t("files:copy") }) as HTMLButtonElement).disabled).toBe(true);
    mocks.listServerFiles.mockResolvedValue({ entries: [{ name: "notes.txt", size: 2, is_dir: false, mod_time: "" }, { name: "world", size: 0, is_dir: true, mod_time: "" }] });
    await userEvent.click(screen.getByRole("button", { name: t("files:refresh") }));
    await userEvent.click(await screen.findByRole("checkbox", { name: label("select_item", "notes.txt") }));
    await userEvent.click(screen.getByRole("button", { name: t("files:copy") }));
    await userEvent.click(screen.getByRole("button", { name: label("open_folder", "world") }));
    await userEvent.click(screen.getByRole("button", { name: i18next.t("files:paste_count", { count: 1 }) }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("world/server.properties")).toBeTruthy();
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:task_overwrite") }));
    expect(mocks.transferServerFiles.mock.calls).toEqual([["lobby", "copy", ["notes.txt"], "world", false], ["lobby", "copy", ["notes.txt"], "world", true]]);
  });

  it("deletes selected entries only after confirming the list", async () => {
    renderFiles();
    await userEvent.click(await screen.findByRole("checkbox", { name: t("files:select_all") }));
    await userEvent.click(screen.getByRole("button", { name: t("files:delete_selected") }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("world")).toBeTruthy();
    expect(within(dialog).getByText("server.properties")).toBeTruthy();
    expect(mocks.deleteServerFile).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:delete") }));
    expect(mocks.deleteServerFile.mock.calls.map((call) => call[1]).sort()).toEqual(["server.properties", "world"]);
  });

  it("keeps paste disabled until the destination directory finishes loading", async () => {
    mocks.listServerFiles.mockResolvedValue({ entries: [{ name: "notes.txt", size: 2, is_dir: false, mod_time: "" }, { name: "world", size: 0, is_dir: true, mod_time: "" }] });
    mocks.transferServerFiles.mockResolvedValue({ op: fileOp({ op: "copy", path: "world", state: "succeeded" }) });
    renderFiles();
    await userEvent.click(await screen.findByRole("checkbox", { name: label("select_item", "notes.txt") }));
    await userEvent.click(screen.getByRole("button", { name: t("files:copy") }));
    let loaded!: (value: unknown) => void;
    mocks.listServerFiles.mockReturnValueOnce(new Promise((resolve) => { loaded = resolve; }));
    await userEvent.click(screen.getByRole("button", { name: label("open_folder", "world") }));
    const paste = screen.getByRole("button", { name: i18next.t("files:paste_count", { count: 1 }) }) as HTMLButtonElement;
    expect(paste.disabled).toBe(true);
    await userEvent.click(paste);
    expect(mocks.transferServerFiles).not.toHaveBeenCalled();
    await act(async () => { loaded({ entries: [], truncated: false, free_bytes: null }); });
    await waitFor(() => expect(paste.disabled).toBe(false));
    await userEvent.click(paste);
    expect(mocks.transferServerFiles).toHaveBeenCalledWith("lobby", "copy", ["notes.txt"], "world", false);
  });

  it("clears the cut selection after an asynchronous move completes", async () => {
    mocks.listServerFiles.mockResolvedValue({ entries: [{ name: "notes.txt", size: 2, is_dir: false, mod_time: "" }, { name: "world", size: 0, is_dir: true, mod_time: "" }] });
    mocks.transferServerFiles.mockResolvedValue({ op: fileOp({ op: "move", path: "world" }) });
    renderFiles();
    await userEvent.click(await screen.findByRole("checkbox", { name: label("select_item", "notes.txt") }));
    await userEvent.click(screen.getByRole("button", { name: t("files:cut") }));
    await userEvent.click(screen.getByRole("button", { name: label("open_folder", "world") }));
    const paste = i18next.t("files:paste_count", { count: 1 });
    await userEvent.click(screen.getByRole("button", { name: paste }));
    expect(mocks.transferServerFiles).toHaveBeenCalledWith("lobby", "move", ["notes.txt"], "world", false);
    opsNow = [fileOp({ op: "move", path: "world", state: "succeeded" })];
    await userEvent.click(screen.getByRole("button", { name: t("files:refresh") }));
    await waitFor(() => expect(screen.queryByRole("button", { name: paste })).toBeNull());
  });

  it("announces a move that finishes after this page started it", async () => {
    mocks.listServerFiles.mockResolvedValue({ entries: [{ name: "notes.txt", size: 2, is_dir: false, mod_time: "" }, { name: "world", size: 0, is_dir: true, mod_time: "" }] });
    mocks.transferServerFiles.mockResolvedValue({ op: fileOp({ op: "move", path: "world" }) });
    renderFiles();
    await userEvent.click(await screen.findByRole("checkbox", { name: label("select_item", "notes.txt") }));
    await userEvent.click(screen.getByRole("button", { name: t("files:cut") }));
    await userEvent.click(screen.getByRole("button", { name: label("open_folder", "world") }));
    await userEvent.click(screen.getByRole("button", { name: i18next.t("files:paste_count", { count: 1 }) }));
    opsNow = [fileOp({ op: "move", path: "world", state: "succeeded" })];
    await userEvent.click(screen.getByRole("button", { name: t("files:refresh") }));
    expect(await within(screen.getByRole("region", { name: t("common:notifications") })).findByText(t("files:op_finished_move"))).toBeTruthy();
  });

  it("starts URL downloads with a world acknowledgement and preserves the entered path", async () => {
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running" });
    mocks.fetchServerFile.mockResolvedValue({ op: fileOp({ op: "upload", path: "world/map.dat", state: "succeeded" }) });
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: t("files:from_url") }));
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByRole("textbox", { name: t("files:url_label") }), "https://example.com/map.dat");
    await userEvent.type(within(dialog).getByRole("textbox", { name: t("files:target_path") }), "world/map.dat");
    const start = within(dialog).getByRole("button", { name: t("files:from_url_start") }) as HTMLButtonElement;
    expect(start.disabled).toBe(true);
    await userEvent.click(within(dialog).getByRole("checkbox", { name: t("files:live_world_confirm") }));
    await userEvent.click(start);
    expect(mocks.fetchServerFile).toHaveBeenCalledWith("lobby", "https://example.com/map.dat", "world/map.dat", false);
  });

  it("opens a keyboard menu from right click and closes it with Escape", async () => {
    renderFiles();
    const file = await screen.findByRole("button", { name: label("open_file", "server.properties") });
    fireEvent.contextMenu(file.closest("tr")!);
    const menu = screen.getByRole("menu", { name: label("more_item", "server.properties") });
    expect(within(menu).getByRole("menuitem", { name: t("files:copy") })).toBeTruthy();
    fireEvent.keyDown(menu, { key: "End" });
    expect(document.activeElement?.textContent).toBe(t("files:delete"));
    fireEvent.keyDown(menu, { key: "Escape" });
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement?.getAttribute("aria-label")).toBe(label("more_item", "server.properties"));
  });

  it("opens the menu on a long press without also opening the file", async () => {
    renderFiles();
    const file = await screen.findByRole("button", { name: label("open_file", "server.properties") });
    const row = file.closest("tr")!;
    vi.useFakeTimers();
    fireEvent.touchStart(row);
    act(() => { vi.advanceTimersByTime(600); });
    expect(screen.getByRole("menu", { name: label("more_item", "server.properties") })).toBeTruthy();
    fireEvent.touchEnd(row);
    fireEvent.click(file);
    expect(mocks.readServerFile).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it.each(["backup.tar", "backup.tar.gz", "backup.tgz"])("extracts %s through the existing archive action", async (name) => {
    mocks.listServerFiles.mockResolvedValue({ entries: [{ name, size: 1024, is_dir: false, mod_time: "" }] });
    mocks.unzipServerFile.mockResolvedValue({ op: fileOp({ path: name, state: "succeeded" }) });
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: label("unzip_item", name) }));
    expect(mocks.unzipServerFile).toHaveBeenCalledWith("lobby", name, false);
  });

  it("accepts UTF-8 text up to 5 MiB and blocks larger saves by encoded size", async () => {
    const { dialog, editor } = await openEditor();
    fireEvent.change(editor, { target: { value: "a".repeat(5 * 1024 * 1024) } });
    expect((within(dialog).getByRole("button", { name: t("files:save") }) as HTMLButtonElement).disabled).toBe(false);
    fireEvent.change(editor, { target: { value: "界".repeat(Math.floor(5 * 1024 * 1024 / 3) + 1) } });
    expect((within(dialog).getByRole("button", { name: t("files:save") }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("previews an image through its export ticket and revokes the object URL when closed", async () => {
    const create = vi.fn(() => "blob:preview");
    const revoke = vi.fn();
    const BrowserURL = URL;
    vi.stubGlobal("URL", class extends BrowserURL { static createObjectURL = create; static revokeObjectURL = revoke; });
    mocks.listServerFiles.mockResolvedValue({ entries: [{ name: "map.png", size: 8, is_dir: false, mod_time: "" }] });
    mocks.downloadServerFile.mockResolvedValue({ ticket: "image-ticket", filename: "map.png", state: "pending" });
    mocks.exportStatus.mockResolvedValue({ state: "ready" });
    mocks.previewServerFile.mockResolvedValue({ arrayBuffer: async () => new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10]).buffer });
    renderFiles();
    const file = await screen.findByText("map.png");
    vi.useFakeTimers();
    fireEvent.click(file);
    await act(async () => { await vi.advanceTimersByTimeAsync(EXPORT_POLL_MS); });
    expect(screen.getByRole("img", { name: i18next.t("files:image_preview", { path: "map.png" }) }).getAttribute("src")).toBe("blob:preview");
    expect(mocks.readServerFile).not.toHaveBeenCalled();
    expect(mocks.previewServerFile).toHaveBeenCalledWith("image-ticket", 5 * 1024 * 1024, expect.any(AbortSignal));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(revoke).toHaveBeenCalledWith("blob:preview");
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });
});

describe("ServerFiles editor", () => {
  it("closes an unchanged file at once", async () => {
    const { dialog } = await openEditor();

    await userEvent.click(within(dialog).getByRole("button", { name: t("common:cancel") }));

    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("asks before dropping edits, and keep editing keeps the text", async () => {
    const { editor } = await openEditor();
    await userEvent.type(editor, "pvp=false");

    await userEvent.keyboard("{Escape}");

    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(t("files:discard_prompt"))).toBeTruthy();
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:keep_editing") }));
    expect(within(dialog).queryByText(t("files:discard_prompt"))).toBeNull();
    expect((within(dialog).getByRole("textbox") as HTMLTextAreaElement).value).toBe("motd=hi\npvp=false");
  });

  it("drops the edits only after discard is confirmed", async () => {
    const { dialog, editor } = await openEditor();
    await userEvent.type(editor, "pvp=false");

    await userEvent.click(within(dialog).getByRole("button", { name: t("common:cancel") }));
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:discard") }));

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(mocks.writeServerFile).not.toHaveBeenCalled();
  });

  it("guards leaving the page while edits are unsaved", async () => {
    const { editor } = await openEditor();
    const clean = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(clean);
    expect(clean.defaultPrevented).toBe(false);

    await userEvent.type(editor, "x");
    const dirty = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(dirty);

    expect(dirty.defaultPrevented).toBe(true);
  });
});

describe("ServerFiles properties form", () => {
  it("opens unchanged properties in the form and blocks invalid saves including keyboard shortcuts", async () => {
    renderFiles();
    await userEvent.click(await screen.findByText("server.properties"));
    const dialog = await screen.findByRole("dialog");
    const input = await within(dialog).findByRole("textbox", { name: t("files:properties_field_view_distance") });
    const save = within(dialog).getByRole("button", { name: t("files:save") });
    expect(save).toHaveProperty("disabled", true);
    fireEvent.change(input, { target: { value: "33" } });
    expect(within(dialog).getByRole("alert").textContent).toBe(i18next.t("files:properties_invalid_integer", { min: 3, max: 32 }));
    fireEvent.keyDown(input, { key: "s", ctrlKey: true });
    expect(save).toHaveProperty("disabled", true);
    expect(mocks.writeServerFile).not.toHaveBeenCalled();
    fireEvent.change(input, { target: { value: "32" } });
    fireEvent.keyDown(input, { key: "s", ctrlKey: true });
    await waitFor(() => expect(mocks.writeServerFile).toHaveBeenCalledExactlyOnceWith("lobby", "server.properties", btoa("motd=hi\nview-distance=32\n"), "abc"));
  });

  it.each(["form", "text"])("loads the latest conflicting file in %s mode and uses its new SHA on the next save", async (mode) => {
    renderFiles();
    await userEvent.click(await screen.findByText("server.properties"));
    const dialog = await screen.findByRole("dialog");
    if (mode === "text") await userEvent.click(await within(dialog).findByRole("radio", { name: t("files:properties_text") }));
    const label = t(mode === "form" ? "files:properties_field_motd" : "files:file_content");
    const edit = async (value: string) => fireEvent.change(await within(dialog).findByRole("textbox", { name: label }), { target: { value: mode === "form" ? value : `motd=${value}\n` } });
    await edit("mine");
    mocks.writeServerFile.mockRejectedValueOnce({ code: "file_changed" });
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:save") }));
    expect(await within(dialog).findByText(t("files:conflict_title"))).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: t("files:save") })).toHaveProperty("disabled", true);
    mocks.readServerFile.mockResolvedValue({ content: btoa("# newer\nmotd=theirs\n"), sha256: "new-sha" });
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:conflict_reload") }));
    await waitFor(() => expect(within(dialog).getByRole("textbox", { name: label })).toHaveProperty("value", mode === "form" ? "theirs" : "# newer\nmotd=theirs\n"));
    expect(within(dialog).queryByText(t("files:conflict_title"))).toBeNull();
    expect(within(dialog).getByRole("button", { name: t("files:save") })).toHaveProperty("disabled", true);
    await edit("after reload");
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:save") }));
    expect(mocks.writeServerFile).toHaveBeenLastCalledWith("lobby", "server.properties", btoa(mode === "form" ? "# newer\nmotd=after reload\n" : "motd=after reload\n"), "new-sha");
  });

  it.each(["form", "text"])("requires explicit overwrite after a conflict in %s mode", async (mode) => {
    renderFiles();
    await userEvent.click(await screen.findByText("server.properties"));
    const dialog = await screen.findByRole("dialog");
    if (mode === "text") await userEvent.click(await within(dialog).findByRole("radio", { name: t("files:properties_text") }));
    const input = await within(dialog).findByRole("textbox", { name: t(mode === "form" ? "files:properties_field_motd" : "files:file_content") });
    fireEvent.change(input, { target: { value: mode === "form" ? "mine" : "motd=mine\n" } });
    mocks.writeServerFile.mockRejectedValueOnce({ code: "file_changed" });
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:save") }));
    const overwrite = await within(dialog).findByRole("button", { name: t("files:conflict_overwrite") });
    expect(input).toHaveProperty("value", mode === "form" ? "mine" : "motd=mine\n");
    fireEvent.keyDown(input, { key: "s", ctrlKey: true });
    expect(mocks.writeServerFile).toHaveBeenCalledTimes(1);
    await userEvent.click(overwrite);
    expect(mocks.writeServerFile).toHaveBeenLastCalledWith("lobby", "server.properties", btoa("motd=mine\n"), undefined);
  });
});

describe("ServerFiles YAML forms", () => {
  function protectedListing() {
    mocks.listServerFiles.mockImplementation((_name: string, path: string) => Promise.resolve({ path, truncated: false, entries: path === "config"
      ? [{ name: "paper-global.yml", size: 900, is_dir: false, mod_time: "" }]
      : [{ name: "config", size: 0, is_dir: true, mod_time: "" }],
    }));
  }

  it("retains ordinary YAML conflict and draft state across modes and reloads the latest SHA", async () => {
    const original = "# kept\nspawn-limits:\n  monsters: 70\n";
    mocks.listServerFiles.mockResolvedValue({ entries: [{ name: "bukkit.yml", size: original.length, is_dir: false, mod_time: "" }] });
    mocks.readServerFile.mockResolvedValue({ content: btoa(original), sha256: "first-sha" });
    mocks.writeServerFile.mockRejectedValueOnce({ code: "file_changed" }).mockResolvedValueOnce({ status: "written" });
    renderFiles();
    await userEvent.click(await screen.findByText("bukkit.yml"));
    const dialog = await screen.findByRole("dialog");
    const label = t("files:config_field_bukkit_monsters");
    fireEvent.change(await within(dialog).findByRole("textbox", { name: label }), { target: { value: "42" } });
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:save") }));
    expect(await within(dialog).findByText(t("files:conflict_title"))).toBeTruthy();
    await userEvent.click(within(dialog).getByRole("radio", { name: t("files:properties_text") }));
    const raw = await within(dialog).findByRole("textbox", { name: t("files:file_content") });
    expect(raw).toHaveProperty("value", original.replace("70", "42"));
    expect(within(dialog).getByText(t("files:conflict_title"))).toBeTruthy();
    mocks.readServerFile.mockResolvedValue({ content: btoa(original.replace("70", "71")), sha256: "latest-sha" });
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:conflict_reload") }));
    await waitFor(() => expect(raw).toHaveProperty("value", original.replace("70", "71")));
    await userEvent.click(within(dialog).getByRole("radio", { name: t("files:properties_form") }));
    const input = await within(dialog).findByRole("textbox", { name: label });
    expect(input).toHaveProperty("value", "71");
    fireEvent.change(input, { target: { value: "45" } });
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:save") }));
    expect(mocks.writeServerFile).toHaveBeenLastCalledWith("lobby", "bukkit.yml", btoa(original.replace("70", "45")), "latest-sha");
  });

  it("requires acknowledgement when config is a running world before patching protected settings", async () => {
    protectedListing();
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
    mocks.readServerFile.mockResolvedValue({ path: "server.properties", content: btoa("level-name=config\n"), sha256: "properties-sha" });
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "config" }) }));
    await userEvent.click(await screen.findByText("paper-global.yml"));
    const dialog = await screen.findByRole("dialog");
    const input = await within(dialog).findByRole("textbox", { name: t("files:config_field_global_watchdog_delay") });
    fireEvent.change(input, { target: { value: "12000" } });
    const save = within(dialog).getByRole("button", { name: t("files:save") });
    expect(save).toHaveProperty("disabled", true);
    fireEvent.keyDown(input, { key: "s", ctrlKey: true });
    expect(mocks.patchServerPaperGlobal).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("checkbox", { name: t("files:live_world_confirm") }));
    await userEvent.click(save);
    expect(mocks.patchServerPaperGlobal).toHaveBeenCalledExactlyOnceWith("lobby", { "watchdog.early-warning-delay": 12000 }, "a".repeat(64));
    expect(mocks.writeServerFile).not.toHaveBeenCalled();
    expect(mocks.readServerFile.mock.calls.map((call) => call[1])).toEqual(["server.properties"]);
  });

  it("does not start a protected read while another operation holds the stopped world's files", async () => {
    protectedListing();
    jobsNow = [{ name: "backup", kind: "backup", state: "running" }];
    renderFiles();
    await screen.findByText(t("files:wait_for_backup"));
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "config" }) }));
    await userEvent.click(await screen.findByText("paper-global.yml"));
    expect(mocks.serverPaperGlobal).not.toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: t("files:paper_global_title") })).toBeNull();
  });
});

describe("ServerFiles on a running server", () => {
  it("lists files while running and keeps following server state", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
    renderFiles();
    expect(await screen.findByText(t("files:live_warning"))).toBeTruthy();
    expect(await screen.findByText("server.properties")).toBeTruthy();
    expect(screen.queryByText(t("files:stopped_required_title"))).toBeNull();

    mocks.status.mockResolvedValue(stopped);
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS));
    expect(await screen.findByText("server.properties")).toBeTruthy();
    expect(screen.queryByText(t("files:stopped_required_title"))).toBeNull();

    const reads = mocks.status.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS * 3));
    expect(mocks.status.mock.calls.length).toBeGreaterThan(reads);
  });

  it("saves configuration while running without acknowledging world changes", async () => {
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
    mocks.writeServerFile.mockResolvedValue({ path: "server.properties", status: "written", sha256: "new" });
    const { dialog, editor } = await openEditor();
    await userEvent.type(editor, "motd=changed");
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:save") }));
    expect(mocks.writeServerFile).toHaveBeenCalledOnce();
    expect(mocks.stop).not.toHaveBeenCalled();
  });

  it("requires a checkbox before deleting a custom world's parent directory", async () => {
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
    mocks.readServerFile.mockResolvedValue({ path: "server.properties", content: btoa("level-name=maps/custom\n"), sha256: "abc" });
    mocks.listServerFiles.mockResolvedValue({ path: "", entries: [{ name: "maps", size: 0, is_dir: true, mod_time: "2026-09-01T00:00:00Z" }] });
    mocks.deleteServerFile.mockResolvedValue(undefined);
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:delete_item", { name: "maps" }) }));
    const dialog = screen.getByRole("dialog");
    const confirm = within(dialog).getByRole("button", { name: t("files:delete") });
    expect(confirm).toHaveProperty("disabled", true);
    expect(mocks.deleteServerFile).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("checkbox", { name: t("files:live_world_confirm") }));
    expect(confirm).toHaveProperty("disabled", false);
    await userEvent.click(confirm);
    expect(mocks.deleteServerFile).toHaveBeenCalledExactlyOnceWith("lobby", "maps");
  });

  it("requires acknowledgement before saving a text file inside the world", async () => {
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
    mocks.writeServerFile.mockResolvedValue({ status: "written", sha256: "new" });
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "world" }) }));
    await userEvent.click(await screen.findByText("server.properties"));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(await within(dialog).findByRole("textbox"), "motd=changed");
    const save = within(dialog).getByRole("button", { name: t("files:save") });
    expect(save).toHaveProperty("disabled", true);
    expect(mocks.writeServerFile).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("checkbox", { name: t("files:live_world_confirm") }));
    await userEvent.click(save);
    expect(mocks.writeServerFile).toHaveBeenCalledWith("lobby", "world/server.properties", expect.any(String), "abc");
  });

  it("keeps the running world's acknowledgement after changing level-name", async () => {
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
    mocks.readServerFile.mockResolvedValue({ path: "server.properties", content: btoa("level-name=old-world\n"), sha256: "abc" });
    mocks.listServerFiles.mockResolvedValue({ path: "", entries: [
      { name: "server.properties", size: 20, is_dir: false, mod_time: "" },
      { name: "old-world", size: 0, is_dir: true, mod_time: "" },
    ] });
    mocks.writeServerFile.mockResolvedValue({ status: "written", sha256: "new" });
    const { dialog, editor } = await openEditor();
    await userEvent.clear(editor);
    await userEvent.type(editor, "level-name=new-world");
    mocks.readServerFile.mockResolvedValue({ path: "server.properties", content: btoa("level-name=new-world\n"), sha256: "new" });
    await userEvent.click(within(dialog).getByRole("button", { name: t("files:save") }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await userEvent.click(screen.getByRole("button", { name: i18next.t("files:delete_item", { name: "old-world" }) }));
    expect(within(screen.getByRole("dialog")).getByRole("button", { name: t("files:delete") })).toHaveProperty("disabled", true);
    expect(mocks.deleteServerFile).not.toHaveBeenCalled();
  });

  it("refuses world uploads until the checkbox is selected", async () => {
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
    mocks.uploadServerFile.mockResolvedValue({ status: "uploaded", sha256: "new" });
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "world" }) }));
    const files = [new File(["notes"], "notes.txt")];
    fireEvent.change(screen.getByTestId("upload-input"), { target: { files } });
    expect(await screen.findByText(t("files:live_world_confirm_required"))).toBeTruthy();
    expect(mocks.uploadServerFile).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("checkbox", { name: t("files:live_world_confirm") }));
    fireEvent.change(screen.getByTestId("upload-input"), { target: { files } });
    await waitFor(() => expect(mocks.uploadServerFile).toHaveBeenCalledWith("lobby", "world/notes.txt", files[0], false, expect.any(Object)));
  });

  it("requires acknowledgement before extracting an archive while running", async () => {
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
    mocks.listServerFiles.mockResolvedValue({ path: "", entries: [{ name: "world.zip", size: 10, is_dir: false, mod_time: "" }] });
    mocks.unzipServerFile.mockResolvedValue({ op: fileOp({ state: "succeeded" }) });
    renderFiles();
    const extract = await screen.findByRole("button", { name: i18next.t("files:unzip_item", { name: "world.zip" }) });
    await userEvent.click(extract);
    expect(await screen.findByText(t("files:live_world_confirm_required"))).toBeTruthy();
    expect(mocks.unzipServerFile).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("checkbox", { name: t("files:live_world_confirm") }));
    await userEvent.click(extract);
    expect(mocks.unzipServerFile).toHaveBeenCalledExactlyOnceWith("lobby", "world.zip", false);
  });

  it("offers a failed start a stop, sent without asking, and nothing that would start it", async () => {
    mocks.stop.mockResolvedValue(undefined);
    // Nobody is on a server that never came up, so even an unreadable count does not ask.
    mocks.status.mockResolvedValue({
      ...stopped,
      phase: "Failed",
      desiredState: "Running",
      ready: false,
      playersOnline: 0,
      playerCountUnknown: true,
    });
    renderFiles();

    await screen.findByText(t("files:stopped_required_title"));
    expect(screen.queryByRole("button", { name: t("servers:retry_start") })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));

    expect(mocks.stop).toHaveBeenCalledWith("lobby");
  });
});

describe("ServerFiles from the keyboard", () => {
  it("opens a folder and then a file without a mouse", async () => {
    renderFiles();
    const folder = await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "world" }) });
    folder.focus();
    expect(document.activeElement).toBe(folder);

    await userEvent.keyboard(" ");
    expect(mocks.listServerFiles).toHaveBeenLastCalledWith("lobby", "world");

    const file = await screen.findByRole("button", { name: i18next.t("files:open_file", { name: "server.properties" }) });
    file.focus();
    expect(document.activeElement).toBe(file);
    await userEvent.keyboard("{Enter}");

    expect(await screen.findByRole("dialog")).toBeTruthy();
    // One press reads the file once: the button's click and the row's are one handler.
    expect(mocks.readServerFile.mock.calls).toEqual([["lobby", "world/server.properties"]]);
  });

  it("still opens from a click anywhere on the row", async () => {
    renderFiles();
    const size = await screen.findByText("8 B");

    await userEvent.click(size);

    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(mocks.readServerFile.mock.calls).toEqual([["lobby", "server.properties"]]);
  });
});

describe("ServerFiles opening feedback", () => {
  it("shows the selected file and loading dialog immediately, and ignores a cancelled read", async () => {
    let resolve!: (value: { content: string; sha256: string }) => void;
    mocks.readServerFile.mockReturnValue(new Promise((r) => { resolve = r; }));
    renderFiles();
    await userEvent.click(await screen.findByText("server.properties"));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(t("files:opening_file"))).toBeTruthy();
    expect(screen.getByRole("row", { selected: true, hidden: true }).textContent).toContain("server.properties");
    await userEvent.keyboard("{Escape}");
    await act(async () => resolve({ content: btoa("late file"), sha256: "abc" }));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it.each([
    ["Limbo.jar", 8 * 1024 * 1024, "binary_title"],
    ["spawn.schem", 6000, "binary_title"],
    ["large.log", 5 * 1024 * 1024 + 1, "preview_large_title"],
  ])("explains why %s cannot be edited without starting a read Job", async (name, size, title) => {
    mocks.listServerFiles.mockResolvedValue({ entries: [{ name, size, is_dir: false, mod_time: "" }] });
    renderFiles();
    await userEvent.click(await screen.findByText(name));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(t(`files:${title}`))).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: t("files:download_file") })).toBeTruthy();
    expect(within(dialog).queryByRole("textbox")).toBeNull();
    expect(within(dialog).queryByRole("button", { name: t("files:save") })).toBeNull();
    expect(mocks.readServerFile).not.toHaveBeenCalled();
  });

  it("detects binary bytes even with an unfamiliar extension", async () => {
    mocks.readServerFile.mockResolvedValue({ content: btoa("a\0b"), sha256: "abc" });
    renderFiles();
    await userEvent.click(await screen.findByText("server.properties"));
    expect(await screen.findByText(t("files:binary_title"))).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: t("files:file_content") })).toBeNull();
  });

  it("reuses recent directories, but an explicit refresh invalidates them", async () => {
    renderFiles();
    await userEvent.click(await screen.findByText("world"));
    await screen.findByRole("button", { name: "world" });
    await userEvent.click(screen.getByRole("button", { name: t("files:up") }));
    expect(mocks.listServerFiles.mock.calls).toEqual([["lobby", ""], ["lobby", "world"]]);
    await userEvent.click(screen.getByRole("button", { name: t("files:refresh") }));
    await userEvent.click(await screen.findByText("world"));
    expect(mocks.listServerFiles.mock.calls).toEqual([["lobby", ""], ["lobby", "world"], ["lobby", ""], ["lobby", "world"]]);
  });
});

describe("ServerFiles folder answers arriving out of order", () => {
  type Listing = { path: string; truncated: boolean; entries: { name: string; size: number; is_dir: boolean; mod_time: string }[] };
  function deferred() {
    let resolve!: (v: Listing) => void;
    let reject!: (e: unknown) => void;
    const promise = new Promise<Listing>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  }
  const entry = (name: string, is_dir = false) => ({ name, size: 8, is_dir, mod_time: "2026-09-01T00:00:00Z" });
  const listing = (path: string, ...names: string[]): Listing => ({ path, truncated: false, entries: names.map((n) => entry(n)) });

  let world: ReturnType<typeof deferred>;
  let plugins: ReturnType<typeof deferred>;
  beforeEach(() => {
    world = deferred();
    plugins = deferred();
    mocks.listServerFiles.mockImplementation((_name: string, path: string) => {
      if (path === "world") return world.promise;
      if (path === "plugins") return plugins.promise;
      return Promise.resolve({ path, truncated: false, entries: [entry("world", true), entry("plugins", true)] });
    });
  });

  const folder = (name: string) => screen.findByRole("button", { name: i18next.t("files:open_folder", { name }) });
  const refresh = () => screen.getByRole("button", { name: i18next.t("files:refresh") }) as HTMLButtonElement;

  it.each([
    ["answers", (d: ReturnType<typeof deferred>) => d.resolve(listing("world", "level.dat"))],
    ["fails", (d: ReturnType<typeof deferred>) => d.reject(new Error("world listing broke"))],
  ])("keeps the folder clicked last when the one left behind %s late", async (_label, settle) => {
    renderFiles();
    await userEvent.click(await folder("world"));
    await userEvent.click(await folder("plugins"));
    await act(async () => plugins.resolve(listing("plugins", "config.yml")));
    expect(await screen.findByText("config.yml")).toBeTruthy();

    await act(async () => settle(world));

    expect(screen.getByText("config.yml")).toBeTruthy();
    expect(screen.queryByText("level.dat")).toBeNull();
    expect(screen.getByRole("button", { name: "plugins" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "world" })).toBeNull();
    expect(refresh().disabled).toBe(false);
  });

  it("keeps loading while the folder clicked last is still on its way", async () => {
    renderFiles();
    await userEvent.click(await folder("world"));
    await userEvent.click(await folder("plugins"));

    await act(async () => world.resolve(listing("world", "level.dat")));
    expect(screen.queryByText("level.dat")).toBeNull();
    expect(refresh().disabled).toBe(true);

    await act(async () => plugins.resolve(listing("plugins", "config.yml")));
    expect(await screen.findByText("config.yml")).toBeTruthy();
    expect(refresh().disabled).toBe(false);
  });
});

const button = (key: string, name?: string) =>
  screen.getByRole("button", { name: i18next.t(`files:${key}`, name === undefined ? {} : { name }) }) as HTMLButtonElement;

describe("ServerFiles changes", () => {
  it("offers no rename for what Felis manages, and one for everything else", async () => {
    renderFiles();
    await screen.findByText("world");

    expect(button("rename_item", "server.properties").disabled).toBe(true);
    expect(button("rename_item", "world").disabled).toBe(false);
    expect(button("delete_item", "server.properties").disabled).toBe(false);
  });

  it("renames within the folder and rereads it", async () => {
    mocks.renameServerFile.mockResolvedValue({ path: "world/world", to: "world/world_old", status: "renamed" });
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "world" }) }));
    await screen.findByRole("button", { name: "world" });
    await userEvent.click(screen.getByRole("button", { name: i18next.t("files:rename_item", { name: "world" }) }));
    const dialog = screen.getByRole("dialog");
    const field = within(dialog).getByRole("textbox") as HTMLInputElement;
    expect(field.value).toBe("world");
    const lists = mocks.listServerFiles.mock.calls.length;

    await userEvent.clear(field);
    await userEvent.type(field, " world_old {Enter}");

    expect(mocks.renameServerFile.mock.calls).toEqual([["lobby", "world/world", "world/world_old"]]);
    expect(await screen.findByText(i18next.t("files:renamed", { from: "world", to: "world_old" }))).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 1);
    expect(mocks.listServerFiles).toHaveBeenLastCalledWith("lobby", "world");
  });

  it("keeps rename off until the name changes", async () => {
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:rename_item", { name: "world" }) }));
    const dialog = screen.getByRole("dialog");
    const submit = within(dialog).getByRole("button", { name: t("files:rename") }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);

    await userEvent.type(within(dialog).getByRole("textbox"), "2");

    expect(submit.disabled).toBe(false);
  });

  it("says why a name cannot be used and sends nothing", async () => {
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: t("files:new_folder") }));
    const dialog = screen.getByRole("dialog");
    const field = within(dialog).getByRole("textbox");
    const submit = within(dialog).getByRole("button", { name: t("files:create") }) as HTMLButtonElement;
    // An empty field has nothing to say yet, and nothing to create.
    expect(within(dialog).queryByText(t("files:name_required"))).toBeNull();
    expect(submit.disabled).toBe(true);

    await userEvent.type(field, "world");
    expect(within(dialog).getByText(t("files:name_taken"))).toBeTruthy();
    expect(submit.disabled).toBe(true);

    await userEvent.clear(field);
    await userEvent.type(field, "a/b{Enter}");
    expect(within(dialog).getByText(t("files:name_slash"))).toBeTruthy();
    expect(field.getAttribute("aria-invalid")).toBe("true");
    expect(mocks.mkdirServerFolder).not.toHaveBeenCalled();
  });

  it("makes a folder in the folder on screen", async () => {
    mocks.mkdirServerFolder.mockResolvedValue({ path: "world/datapacks", status: "created" });
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "world" }) }));
    await screen.findByRole("button", { name: "world" });

    const lists = mocks.listServerFiles.mock.calls.length;

    await userEvent.click(screen.getByRole("button", { name: t("files:new_folder") }));
    await userEvent.type(within(screen.getByRole("dialog")).getByRole("textbox"), "datapacks{Enter}");

    expect(mocks.mkdirServerFolder.mock.calls).toEqual([["lobby", "world/datapacks"]]);
    expect(await screen.findByText(i18next.t("files:folder_created", { path: "world/datapacks" }))).toBeTruthy();
    expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 1);
    expect(mocks.listServerFiles).toHaveBeenLastCalledWith("lobby", "world");
  });

  it("sends a name once, and stays open until the answer comes", async () => {
    let answer!: (v: unknown) => void;
    mocks.mkdirServerFolder.mockReturnValue(new Promise((res) => (answer = res)));
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: t("files:new_folder") }));
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByRole("textbox"), "datapacks");
    const create = within(dialog).getByRole("button", { name: t("files:create") });

    await userEvent.click(create);
    await userEvent.click(create);
    await userEvent.keyboard("{Escape}");

    expect(mocks.mkdirServerFolder).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("dialog")).toBe(dialog);
    await act(async () => answer({ path: "datapacks", status: "created" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("keeps the dialog open with the server's refusal", async () => {
    const refusal = { status: 409, code: "file_exists", message: "something is already at datapacks" };
    mocks.mkdirServerFolder.mockRejectedValue(refusal);
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: t("files:new_folder") }));
    const dialog = screen.getByRole("dialog");

    await userEvent.type(within(dialog).getByRole("textbox"), "datapacks{Enter}");

    expect(await within(dialog).findByText(humanizeError(refusal))).toBeTruthy();
    expect(screen.getByRole("dialog")).toBe(dialog);
  });

  it("opens a new file in the editor, and its first save expects the file it made", async () => {
    mocks.createServerFile.mockResolvedValue({ path: "notes.txt", status: "written", sha256: "e".repeat(64) });
    mocks.writeServerFile.mockResolvedValue({ path: "notes.txt", status: "written", sha256: "f".repeat(64) });
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: t("files:new_file") }));

    await userEvent.type(within(screen.getByRole("dialog")).getByRole("textbox"), "notes.txt{Enter}");

    expect(mocks.createServerFile.mock.calls).toEqual([["lobby", "notes.txt", ""]]);
    const editor = await screen.findByRole("dialog");
    expect(within(editor).getByText("notes.txt")).toBeTruthy();
    await userEvent.type(within(editor).getByRole("textbox"), "hi");
    await userEvent.click(within(editor).getByRole("button", { name: t("files:save") }));
    expect(mocks.writeServerFile.mock.calls).toEqual([["lobby", "notes.txt", btoa("hi"), "e".repeat(64)]]);
  });

  it("asks before deleting a folder with everything in it, then rereads", async () => {
    mocks.deleteServerFile.mockResolvedValue({ path: "world/world", status: "deleted" });
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "world" }) }));
    await screen.findByRole("button", { name: "world" });
    await userEvent.click(screen.getByRole("button", { name: i18next.t("files:delete_item", { name: "world" }) }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(i18next.t("files:delete_folder_title", { name: "world" }))).toBeTruthy();
    expect(within(dialog).getByText(t("files:delete_folder_body"))).toBeTruthy();
    expect(mocks.deleteServerFile).not.toHaveBeenCalled();
    const lists = mocks.listServerFiles.mock.calls.length;

    await userEvent.click(within(dialog).getByRole("button", { name: t("files:delete") }));

    expect(mocks.deleteServerFile.mock.calls).toEqual([["lobby", "world/world"]]);
    expect(await screen.findByText(i18next.t("files:deleted", { path: "world/world" }))).toBeTruthy();
    expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 1);
    expect(mocks.listServerFiles).toHaveBeenLastCalledWith("lobby", "world");
  });

  it("words a file's deletion as a file's", async () => {
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:delete_item", { name: "server.properties" }) }));
    const dialog = screen.getByRole("dialog");

    expect(within(dialog).getByText(i18next.t("files:delete_file_title", { name: "server.properties" }))).toBeTruthy();
    expect(within(dialog).getByText(t("files:delete_file_body"))).toBeTruthy();
    // The row's own click never fired: the file did not open behind it.
    expect(mocks.readServerFile).not.toHaveBeenCalled();
  });

  it("opens the protected scalar form without reading raw proxy configuration", async () => {
    mocks.listServerFiles.mockImplementation((_name: string, path: string) =>
      Promise.resolve({
        path,
        truncated: false,
        entries:
          path === "config"
            ? [{ name: "paper-global.yml", size: 900, is_dir: false, mod_time: "2026-09-01T00:00:00Z" }]
            : [{ name: "config", size: 0, is_dir: true, mod_time: "2026-09-01T00:00:00Z" }],
      }),
    );
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "config" }) }));

    expect(button("rename_item", "paper-global.yml").disabled).toBe(true);
    await userEvent.click(await screen.findByText("paper-global.yml"));

    expect(await screen.findByRole("heading", { name: t("files:paper_global_title") })).toBeTruthy();
    expect(mocks.serverPaperGlobal).toHaveBeenCalledExactlyOnceWith("lobby");
    expect(screen.queryByRole("radio", { name: t("files:properties_text") })).toBeNull();
    expect(mocks.readServerFile).not.toHaveBeenCalled();
  });

  it("lists folders first and numbers in counting order", async () => {
    mocks.listServerFiles.mockResolvedValue({
      path: "",
      truncated: false,
      entries: ["r.10.0.mca", "world", "r.2.0.mca", "logs"].map((name) => ({
        name,
        size: 1,
        is_dir: !name.endsWith(".mca"),
        mod_time: "2026-09-01T00:00:00Z",
      })),
    });
    renderFiles();
    await screen.findByText("logs");

    const names = screen
      .getAllByRole("button", { name: /^(Open|打开)/ })
      .map((b) => b.textContent);
    expect(names).toEqual(["logs", "world", "r.2.0.mca", "r.10.0.mca"]);
  });
});

describe("ServerFiles uploads", () => {
  type Pending = {
    resolve: () => void;
    reject: (e: unknown) => void;
    signal?: AbortSignal;
    progress?: (sent: number) => void;
  };
  let pending: Pending[];
  beforeEach(() => {
    pending = [];
    mocks.uploadServerFile.mockImplementation(
      (
        _name: string,
        path: string,
        file: File,
        _overwrite: boolean,
        opts?: { signal?: AbortSignal; onProgress?: (sent: number) => void },
      ) =>
        new Promise((resolve, reject) => {
          pending.push({
            resolve: () => resolve({ path, status: "uploaded", sha256: "a", size: file.size }),
            reject,
            signal: opts?.signal,
            progress: opts?.onProgress,
          });
          opts?.signal?.addEventListener("abort", () => reject(new DOMException("cancelled", "AbortError")));
        }),
    );
  });

  const file = (name: string, body = "bytes") => new File([body], name);
  const pick = (...files: File[]) => fireEvent.change(screen.getByTestId("upload-input"), { target: { files } });
  const queue = () => screen.getByRole("region", { name: t("files:uploads_label") });
  const sentAs = () => mocks.uploadServerFile.mock.calls.map((c) => [c[1], c[3]]);

  it("sends one at a time and rereads the folder once the queue is empty", async () => {
    renderFiles();
    await screen.findByText("world");
    const lists = mocks.listServerFiles.mock.calls.length;

    pick(file("a.jar"), file("b.jar"));

    await waitFor(() => expect(sentAs()).toEqual([["a.jar", false]]));
    expect(within(queue()).getByText(t("files:upload_queued"))).toBeTruthy();
    await act(async () => pending[0].resolve());
    await waitFor(() => expect(sentAs()).toEqual([["a.jar", false], ["b.jar", false]]));
    expect(mocks.listServerFiles.mock.calls.length).toBe(lists);

    await act(async () => pending[1].resolve());

    await waitFor(() => expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 1));
    expect(within(queue()).getAllByText(t("files:upload_done"))).toHaveLength(2);
  });

  it("rereads once at the end even when the folder changes on the way", async () => {
    renderFiles();
    await screen.findByText("world");
    pick(file("a.jar"), file("b.jar"));
    await waitFor(() => expect(pending).toHaveLength(1));
    await act(async () => pending[0].resolve());
    await waitFor(() => expect(pending).toHaveLength(2));
    const lists = mocks.listServerFiles.mock.calls.length;

    await userEvent.click(screen.getByRole("button", { name: i18next.t("files:open_folder", { name: "world" }) }));
    await screen.findByRole("button", { name: "world" });
    expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 1);

    await act(async () => pending[1].resolve());
    await waitFor(() => expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 2));
    expect(mocks.listServerFiles).toHaveBeenLastCalledWith("lobby", "world");

    // That reread was the landing's: going back is one listing, as always.
    await userEvent.click(screen.getByRole("button", { name: t("files:up") }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "world" })).toBeNull());
    expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 3);
  });

  it("lands in the folder on screen when it was added", async () => {
    renderFiles();
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "world" }) }));
    await screen.findByRole("button", { name: "world" });

    pick(file("level.dat_old"));

    await waitFor(() => expect(sentAs()).toEqual([["world/level.dat_old", false]]));
  });

  it("waits on a file already there until told to replace it", async () => {
    renderFiles();
    await screen.findByText("world");

    pick(file("server.properties"));

    expect(await within(queue()).findByText(t("files:upload_exists"))).toBeTruthy();
    expect(mocks.uploadServerFile).not.toHaveBeenCalled();
    await userEvent.click(within(queue()).getByRole("button", { name: t("files:upload_replace") }));
    await waitFor(() => expect(sentAs()).toEqual([["server.properties", true]]));
  });

  it("drops a skipped file without sending it", async () => {
    renderFiles();
    await screen.findByText("world");
    pick(file("server.properties"));

    await userEvent.click(await within(queue()).findByRole("button", { name: t("files:upload_skip") }));

    expect(screen.queryByRole("region", { name: t("files:uploads_label") })).toBeNull();
    expect(mocks.uploadServerFile).not.toHaveBeenCalled();
  });

  const sized = (name: string, size: number) => {
    const f = file(name);
    Object.defineProperty(f, "size", { value: size });
    return f;
  };
  const withFree = (free: number | null) =>
    mocks.listServerFiles.mockImplementation((_name: string, path: string) =>
      Promise.resolve({
        path,
        truncated: false,
        free_bytes: free,
        entries: [{ name: "world", size: 0, is_dir: true, mod_time: "2026-09-01T00:00:00Z" }],
      }),
    );

  it("refuses a name a folder holds without sending or offering a retry", async () => {
    renderFiles();
    await screen.findByText("world");

    pick(file("world"));

    expect(await within(queue()).findByText(t("files:upload_folder_there"))).toBeTruthy();
    expect(within(queue()).queryByRole("button", { name: t("files:upload_retry") })).toBeNull();
    expect(mocks.uploadServerFile).not.toHaveBeenCalled();
  });

  it("refuses a name past 255 bytes without sending it, and sends one at 255", async () => {
    renderFiles();
    await screen.findByText("world");

    // A Mac counts 86 characters, the server 258 bytes; 85 of them are 255.
    pick(file("界".repeat(86)), file("界".repeat(85)));

    expect(await within(queue()).findByText(t("files:upload_name_too_long"))).toBeTruthy();
    expect(within(queue()).queryByRole("button", { name: t("files:upload_retry") })).toBeNull();
    await waitFor(() => expect(sentAs()).toEqual([["界".repeat(85), false]]));
  });

  it("refuses what the volume has no room for, counting the files ahead in the batch, and retries once there is room", async () => {
    withFree(100);
    renderFiles();
    await screen.findByText("world");

    pick(sized("a.jar", 60), sized("b.jar", 50), sized("c.jar", 40));

    expect(await within(queue()).findByText(i18next.t("files:upload_no_room", { free: "40 B", size: "50 B" }))).toBeTruthy();
    await waitFor(() => expect(sentAs()).toEqual([["a.jar", false]]));
    await act(async () => pending[0].resolve());
    await waitFor(() => expect(sentAs()).toEqual([["a.jar", false], ["c.jar", false]]));
    await act(async () => pending[1].resolve());
    await waitFor(() => expect(within(queue()).getAllByText(t("files:upload_done"))).toHaveLength(2));

    // The listing after they landed still says 100 B free.
    await userEvent.click(within(queue()).getByRole("button", { name: t("files:upload_retry") }));
    await waitFor(() => expect(sentAs()).toEqual([["a.jar", false], ["c.jar", false], ["b.jar", false]]));
  });

  it("sends when the listing could not tell how much room there is", async () => {
    withFree(null);
    renderFiles();
    await screen.findByText("world");

    pick(sized("a.jar", 60));

    await waitFor(() => expect(sentAs()).toEqual([["a.jar", false]]));
  });

  it("refuses everything but an empty file on a full volume", async () => {
    withFree(0);
    renderFiles();
    await screen.findByText("world");

    pick(sized("a.jar", 1), sized("empty.txt", 0));

    expect(await within(queue()).findByText(i18next.t("files:upload_no_room", { free: "0 B", size: "1 B" }))).toBeTruthy();
    await waitFor(() => expect(sentAs()).toEqual([["empty.txt", false]]));
  });

  it("keeps a retry refused while the latest listing has no room for the file", async () => {
    withFree(10);
    renderFiles();
    await screen.findByText("world");
    pick(sized("a.jar", 60));
    await within(queue()).findByText(i18next.t("files:upload_no_room", { free: "10 B", size: "60 B" }));

    withFree(20);
    await userEvent.click(screen.getByRole("button", { name: t("files:refresh") }));
    await waitFor(() => expect(mocks.listServerFiles).toHaveBeenCalledTimes(2));
    await userEvent.click(within(queue()).getByRole("button", { name: t("files:upload_retry") }));

    expect(await within(queue()).findByText(i18next.t("files:upload_no_room", { free: "20 B", size: "60 B" }))).toBeTruthy();
    expect(mocks.uploadServerFile).not.toHaveBeenCalled();
  });

  it("sends a file of exactly one request's worth in one request", async () => {
    renderFiles();
    await screen.findByText("world");

    pick(sized("edge.zip", ONE_REQUEST_BYTES));

    await waitFor(() => expect(sentAs()).toEqual([["edge.zip", false]]));
    expect(mocks.beginServerFileUpload).not.toHaveBeenCalled();
  });

  describe("a file over one request's worth", () => {
    const PART = 32 * 1024 * 1024;
    const SIZE = ONE_REQUEST_BYTES + 1;
    const KEY = "felis-file-upload:lobby:world.zip";
    // sized() keeps the five bytes file() made, and a slice past them is empty:
    // those are the bytes a resume hashes the held parts against.
    const held = (received: number) => {
      const out: { size: number; sha256: string }[] = [];
      for (let start = 0; start < received; start += PART) {
        const end = Math.min(start + PART, received);
        out.push({ size: end - start, sha256: createHash("sha256").update("bytes".slice(start, end)).digest("hex") });
      }
      return out;
    };
    const at = (received: number) => ({
      id: "s1",
      path: "world.zip",
      size: SIZE,
      received,
      part_max_bytes: PART,
      parts: held(received),
    });
    let parts: { offset: number; signal?: AbortSignal }[];

    beforeEach(() => {
      parts = [];
      mocks.beginServerFileUpload.mockResolvedValue(at(0));
      mocks.putServerFileUploadPart.mockImplementation(
        async (_n: string, _id: string, offset: number, _part: Blob, opts?: { signal?: AbortSignal }) => {
          parts.push({ offset, signal: opts?.signal });
          return at(Math.min(offset + PART, SIZE));
        },
      );
      mocks.commitServerFileUpload.mockResolvedValue({ op: fileOp({ id: "land1", op: "upload", path: "world.zip" }) });
      mocks.deleteServerFileUpload.mockResolvedValue(null);
    });

    it("goes up in parts, shows the Job writing it, and rereads once it lands", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      renderFiles();
      await screen.findByText("world");
      const lists = mocks.listServerFiles.mock.calls.length;

      pick(sized("world.zip", SIZE));

      await waitFor(() => expect(mocks.commitServerFileUpload.mock.calls).toEqual([["lobby", "s1", false]]));
      expect(parts.map((p) => p.offset)).toEqual([0, PART, 2 * PART]);
      expect(mocks.uploadServerFile).not.toHaveBeenCalled();
      expect(await within(queue()).findByText(t("files:upload_landing"))).toBeTruthy();

      opsNow = [fileOp({ id: "land1", op: "upload", path: "world.zip", done: SIZE / 4, total: SIZE })];
      await act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));
      expect(within(queue()).getByText(i18next.t("files:upload_landing_progress", { percent: 25 }))).toBeTruthy();
      expect(within(queue()).getByRole("progressbar").getAttribute("aria-valuenow")).toBe("25");
      // Its own row follows it; the list of background operations leaves it out.
      fireEvent.click(screen.getByRole("button", { name: t("files:refresh") }));
      await waitFor(() => expect(mocks.listServerFileOps.mock.calls.length).toBeGreaterThanOrEqual(2));
      expect(screen.queryByRole("region", { name: t("files:ops_label") })).toBeNull();
      const before = mocks.listServerFiles.mock.calls.length;

      opsNow = [fileOp({ id: "land1", op: "upload", path: "world.zip", state: "succeeded", done: SIZE, total: SIZE })];
      await act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));

      expect(await within(queue()).findByText(t("files:upload_done"))).toBeTruthy();
      await waitFor(() => expect(mocks.listServerFiles.mock.calls.length).toBe(before + 1));
      expect(before).toBe(lists + 1); // the refresh
      expect(localStorage.getItem(KEY)).toBeNull();
    });

    it("asks for world confirmation if the server starts before parts are committed", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      let release!: (value: ReturnType<typeof at>) => void;
      mocks.putServerFileUploadPart.mockImplementationOnce(() => new Promise((resolve) => { release = resolve; }));
      mocks.commitServerFileUpload.mockResolvedValue({ op: fileOp({ op: "upload", path: "world/world.zip", state: "succeeded" }) });
      renderFiles();
      await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "world" }) }));
      pick(sized("world.zip", SIZE));
      await waitFor(() => expect(mocks.beginServerFileUpload).toHaveBeenCalledWith("lobby", "world/world.zip", SIZE));
      await waitFor(() => expect(mocks.putServerFileUploadPart).toHaveBeenCalledOnce());

      mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
      await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS));
      await screen.findByText(t("files:live_warning"));
      await act(async () => release(at(PART)));
      expect(await within(queue()).findByText(t("files:live_world_confirm_required"))).toBeTruthy();
      expect(mocks.commitServerFileUpload).not.toHaveBeenCalled();

      mocks.getServerFileUpload.mockResolvedValue({ ...at(SIZE), path: "world/world.zip" });
      await userEvent.click(screen.getByRole("checkbox", { name: t("files:live_world_confirm") }));
      await userEvent.click(within(queue()).getByRole("button", { name: t("files:upload_retry") }));
      expect(await within(queue()).findByText(t("files:upload_done"))).toBeTruthy();
      expect(mocks.commitServerFileUpload).toHaveBeenCalledExactlyOnceWith("lobby", "s1", false);
      expect(mocks.putServerFileUploadPart).toHaveBeenCalledTimes(3);
    });

    it("says why the Job landing it failed, and offers a retry", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      renderFiles();
      await screen.findByText("world");
      pick(sized("world.zip", SIZE));
      await waitFor(() => expect(mocks.commitServerFileUpload).toHaveBeenCalled());

      opsNow = [
        fileOp({
          id: "land1",
          op: "upload",
          path: "world.zip",
          state: "failed",
          error: { code: "volume_full", message: "", need: 2048, avail: 1024 },
        }),
      ];
      await act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));

      expect(
        await within(queue()).findByText(i18next.t("files:op_volume_full", { need: "2.0 KiB", avail: "1.0 KiB" })),
      ).toBeTruthy();
      expect(within(queue()).getByRole("button", { name: t("files:upload_retry") })).toBeTruthy();
    });

    it("asks to replace when the Job found a file there, and lands the bytes already sent", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      renderFiles();
      await screen.findByText("world");
      pick(sized("world.zip", SIZE));
      await waitFor(() => expect(mocks.commitServerFileUpload).toHaveBeenCalledTimes(1));
      opsNow = [
        fileOp({ id: "land1", op: "upload", path: "world.zip", state: "failed", error: { code: "file_exists", message: "" } }),
      ];
      await act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));
      mocks.getServerFileUpload.mockResolvedValue(at(SIZE));
      mocks.commitServerFileUpload.mockResolvedValue({
        op: fileOp({ id: "land2", op: "upload", path: "world.zip", state: "succeeded" }),
      });

      fireEvent.click(await within(queue()).findByRole("button", { name: t("files:upload_replace") }));

      await waitFor(() => expect(mocks.commitServerFileUpload).toHaveBeenLastCalledWith("lobby", "s1", true));
      expect(parts).toHaveLength(3);
      expect(mocks.beginServerFileUpload).toHaveBeenCalledTimes(1);
      expect(await within(queue()).findByText(t("files:upload_done"))).toBeTruthy();
    });

    it.each([
      ["dismissed", { code: "volume_full", message: "" }, "upload_dismiss", { name: "world.zip" }],
      ["skipped", { code: "file_exists", message: "" }, "upload_skip", {}],
    ])("gives the session back when a refused one is %s", async (_label, error, key, opts) => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      renderFiles();
      await screen.findByText("world");
      pick(sized("world.zip", SIZE));
      await waitFor(() => expect(mocks.commitServerFileUpload).toHaveBeenCalledTimes(1));
      opsNow = [fileOp({ id: "land1", op: "upload", path: "world.zip", state: "failed", error })];
      await act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));

      fireEvent.click(await within(queue()).findByRole("button", { name: i18next.t(`files:${key}`, opts) }));

      await waitFor(() => expect(mocks.deleteServerFileUpload.mock.calls).toEqual([["lobby", "s1"]]));
      expect(localStorage.getItem(KEY)).toBeNull();
      expect(screen.queryByRole("region", { name: t("files:uploads_label") })).toBeNull();
    });

    it("gives the session back when skipped with the rest", async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      renderFiles();
      await screen.findByText("world");
      pick(sized("world.zip", SIZE));
      await waitFor(() => expect(mocks.commitServerFileUpload).toHaveBeenCalledTimes(1));
      opsNow = [
        fileOp({ id: "land1", op: "upload", path: "world.zip", state: "failed", error: { code: "file_exists", message: "" } }),
      ];
      await act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));
      await within(queue()).findByRole("button", { name: t("files:upload_replace") });
      pick(file("server.properties"));

      fireEvent.click(await within(queue()).findByRole("button", { name: t("files:upload_skip_all") }));

      await waitFor(() => expect(mocks.deleteServerFileUpload.mock.calls).toEqual([["lobby", "s1"]]));
      expect(screen.queryByRole("region", { name: t("files:uploads_label") })).toBeNull();
    });

    it("gives the session back when cancelled", async () => {
      mocks.putServerFileUploadPart.mockImplementation(
        (_n: string, _id: string, offset: number, _part: Blob, opts?: { signal?: AbortSignal }) =>
          new Promise((_resolve, reject) => {
            parts.push({ offset, signal: opts?.signal });
            opts?.signal?.addEventListener("abort", () => reject(new DOMException("cancelled", "AbortError")));
          }),
      );
      renderFiles();
      await screen.findByText("world");
      pick(sized("world.zip", SIZE));
      await waitFor(() => expect(parts).toHaveLength(1));
      expect(localStorage.getItem(KEY)).not.toBeNull();

      await userEvent.click(within(queue()).getByRole("button", { name: i18next.t("files:upload_cancel", { name: "world.zip" }) }));

      await waitFor(() => expect(mocks.deleteServerFileUpload.mock.calls).toEqual([["lobby", "s1"]]));
      expect(localStorage.getItem(KEY)).toBeNull();
    });

    it("keeps the session for a resume when the page goes away", async () => {
      mocks.putServerFileUploadPart.mockImplementation(
        (_n: string, _id: string, offset: number, _part: Blob, opts?: { signal?: AbortSignal }) =>
          new Promise((_resolve, reject) => {
            parts.push({ offset, signal: opts?.signal });
            opts?.signal?.addEventListener("abort", () => reject(new DOMException("cancelled", "AbortError")));
          }),
      );
      const { unmount } = renderFiles();
      await screen.findByText("world");
      pick(sized("world.zip", SIZE));
      await waitFor(() => expect(parts).toHaveLength(1));

      unmount();
      await act(async () => {});

      expect(parts[0].signal?.aborted).toBe(true);
      expect(mocks.deleteServerFileUpload).not.toHaveBeenCalled();
      expect(JSON.parse(localStorage.getItem(KEY) ?? "null")).toMatchObject({ id: "s1", size: SIZE });
    });
  });

  it("holds the queue and every change while a background op runs, then carries on", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    opsNow = [fileOp()];
    renderFiles();
    const ops = await screen.findByRole("region", { name: t("files:ops_label") });
    expect(within(ops).getByText(i18next.t("files:op_unzip", { path: "pack.zip" }))).toBeTruthy();
    expect(within(ops).getByText(t("files:op_preparing"))).toBeTruthy();
    const lists = mocks.listServerFiles.mock.calls.length;

    pick(file("a.jar"));
    expect(await within(queue()).findByText(t("files:upload_queued"))).toBeTruthy();
    for (const b of [button("new_file"), button("new_folder"), button("rename_item", "world"), button("delete_item", "world")]) {
      expect(b.disabled).toBe(true);
      expect(b.title).toBe(t("files:wait_for_op"));
    }

    opsNow = [fileOp({ done: 512, total: 2048 })];
    await act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));
    expect(within(ops).getByText(i18next.t("files:op_progress", { done: "512 B", total: "2.0 KiB", percent: 25 }))).toBeTruthy();
    expect(within(ops).getByRole("progressbar").getAttribute("aria-valuenow")).toBe("25");
    expect(mocks.uploadServerFile).not.toHaveBeenCalled();

    opsNow = [fileOp({ state: "succeeded", files: 3, bytes: 2048, done: 2048, total: 2048 })];
    await act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));

    await waitFor(() => expect(sentAs()).toEqual([["a.jar", false]]));
    expect(within(ops).getByText(i18next.t("files:op_unzip_done", { count: 3, bytes: "2.0 KiB" }))).toBeTruthy();
    // What it extracted is in the folder now.
    await waitFor(() => expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 1));

    fireEvent.click(within(ops).getByRole("button", { name: i18next.t("files:op_dismiss", { path: "pack.zip" }) }));
    expect(screen.queryByRole("region", { name: t("files:ops_label") })).toBeNull();
  });

  it("asks about a file that appeared since the listing", async () => {
    renderFiles();
    await screen.findByText("world");
    pick(file("icon.png"));
    await waitFor(() => expect(pending).toHaveLength(1));

    await act(async () => pending[0].reject({ status: 409, code: "file_exists", message: "something is already at icon.png" }));

    expect(await within(queue()).findByRole("button", { name: t("files:upload_replace") })).toBeTruthy();
    expect(within(queue()).getByText(t("files:upload_exists"))).toBeTruthy();
  });

  it("offers a retry after a failure and sends it again", async () => {
    const full = { status: 507, code: "volume_full", message: "no space left" };
    renderFiles();
    await screen.findByText("world");
    pick(file("big.jar"));
    await waitFor(() => expect(pending).toHaveLength(1));

    await act(async () => pending[0].reject(full));
    expect(await within(queue()).findByText(humanizeError(full))).toBeTruthy();
    await userEvent.click(within(queue()).getByRole("button", { name: t("files:upload_retry") }));

    await waitFor(() => expect(sentAs()).toEqual([["big.jar", false], ["big.jar", false]]));
  });

  it("cancels the upload in flight and rereads nothing", async () => {
    renderFiles();
    await screen.findByText("world");
    const lists = mocks.listServerFiles.mock.calls.length;
    pick(file("a.jar"));
    await waitFor(() => expect(pending).toHaveLength(1));

    await userEvent.click(within(queue()).getByRole("button", { name: i18next.t("files:upload_cancel", { name: "a.jar" }) }));

    expect(pending[0].signal?.aborted).toBe(true);
    await waitFor(() => expect(screen.queryByRole("region", { name: t("files:uploads_label") })).toBeNull());
    expect(mocks.listServerFiles.mock.calls.length).toBe(lists);
  });

  it("stops the upload in flight when the page goes away", async () => {
    const { unmount } = renderFiles();
    await screen.findByText("world");
    pick(file("a.jar"));
    await waitFor(() => expect(pending).toHaveLength(1));

    unmount();

    expect(pending[0].signal?.aborted).toBe(true);
  });

  it("holds every other change, and leaving the page, while an upload runs", async () => {
    renderFiles();
    await screen.findByText("world");
    pick(file("a.jar"));
    await waitFor(() => expect(pending).toHaveLength(1));

    for (const b of [button("new_file"), button("new_folder"), button("rename_item", "world"), button("delete_item", "world")]) {
      expect(b.disabled).toBe(true);
    }
    const leave = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(leave);
    expect(leave.defaultPrevented).toBe(true);

    await act(async () => pending[0].resolve());

    await waitFor(() => expect(button("new_file").disabled).toBe(false));
    expect(button("delete_item", "world").disabled).toBe(false);
  });

  describe("with several files already there", () => {
    beforeEach(() => {
      mocks.listServerFiles.mockImplementation((_name: string, path: string) =>
        Promise.resolve({
          path,
          truncated: false,
          entries: ["a.yml", "b.yml"].map((name) => ({ name, size: 8, is_dir: false, mod_time: "2026-09-01T00:00:00Z" })),
        }),
      );
    });

    it("replaces every waiting file at once", async () => {
      renderFiles();
      await screen.findByText("a.yml");
      pick(file("a.yml"), file("b.yml"), file("c.yml"));
      await waitFor(() => expect(sentAs()).toEqual([["c.yml", false]]));

      await userEvent.click(within(queue()).getByRole("button", { name: t("files:upload_replace_all") }));
      await act(async () => pending[0].resolve());
      await waitFor(() => expect(pending).toHaveLength(2));
      await act(async () => pending[1].resolve());

      await waitFor(() => expect(sentAs()).toEqual([["c.yml", false], ["a.yml", true], ["b.yml", true]]));
    });

    it("skips every waiting file at once and keeps the rest", async () => {
      renderFiles();
      await screen.findByText("a.yml");
      pick(file("a.yml"), file("b.yml"), file("c.yml"));
      await waitFor(() => expect(pending).toHaveLength(1));

      await userEvent.click(within(queue()).getByRole("button", { name: t("files:upload_skip_all") }));

      expect(within(queue()).queryByText(t("files:upload_exists"))).toBeNull();
      expect(within(queue()).getByText("c.yml")).toBeTruthy();
      expect(sentAs()).toEqual([["c.yml", false]]);
    });
  });

  it("clears the finished uploads and keeps the ones still asking", async () => {
    renderFiles();
    await screen.findByText("world");
    pick(file("a.jar"), file("server.properties"));
    await waitFor(() => expect(pending).toHaveLength(1));
    await act(async () => pending[0].resolve());
    await within(queue()).findByText(t("files:upload_done"));

    await userEvent.click(within(queue()).getByRole("button", { name: t("files:upload_clear_done") }));

    expect(within(queue()).queryByText("a.jar")).toBeNull();
    expect(within(queue()).getByText("server.properties")).toBeTruthy();
  });

  it("shows the file being written once all of it is sent, and no longer offers a cancel", async () => {
    renderFiles();
    await screen.findByText("world");
    pick(file("a.jar", "0123456789"));
    await waitFor(() => expect(pending).toHaveLength(1));
    const cancel = i18next.t("files:upload_cancel", { name: "a.jar" });

    await act(async () => pending[0].progress?.(4));
    expect(within(queue()).getByRole("progressbar").getAttribute("aria-valuenow")).toBe("40");
    expect(within(queue()).getByRole("button", { name: cancel })).toBeTruthy();

    await act(async () => pending[0].progress?.(10));
    expect(within(queue()).getByText(t("files:upload_landing"))).toBeTruthy();
    expect(within(queue()).queryByRole("button", { name: cancel })).toBeNull();
  });

  it("recursively uploads a dropped folder and preserves its relative paths", async () => {
    renderFiles();
    const table = await screen.findByRole("table");
    let read = false;
    const nested = file("config.yml");
    const dataTransfer = {
      types: ["Files"],
      files: [],
      items: [
        { kind: "file", webkitGetAsEntry: () => ({ name: "plugin", isDirectory: true, isFile: false, createReader: () => ({ readEntries: (done: (entries: unknown[]) => void) => {
          done(read ? [] : [{ name: "config.yml", isDirectory: false, isFile: true, file: (got: (file: File) => void) => got(nested) }]); read = true;
        } }) }) },
      ],
    };

    fireEvent.drop(table, { dataTransfer });

    await waitFor(() => expect(sentAs()).toEqual([["plugin/config.yml", false]]));
    expect(mocks.mkdirServerFolder).toHaveBeenCalledWith("lobby", "plugin");
  });

  it("keeps a file dropped beside the list from replacing the page", async () => {
    renderFiles();
    const table = await screen.findByRole("table");
    const beside = { types: ["Files"], dropEffect: "move" };
    const onList = { types: ["Files"], dropEffect: "move" };

    expect(fireEvent.dragOver(document.body, { dataTransfer: beside })).toBe(false);
    expect(beside.dropEffect).toBe("none");
    expect(fireEvent.dragOver(table, { dataTransfer: onList })).toBe(false);
    expect(onList.dropEffect).toBe("copy");
    // Dragging text is the browser's own business.
    expect(fireEvent.dragOver(document.body, { dataTransfer: { types: ["text/plain"], dropEffect: "move" } })).toBe(true);
  });
});

describe("ServerFiles archives and downloads", () => {
  const entry = (name: string, is_dir = false) => ({ name, size: 8, is_dir, mod_time: "2026-09-01T00:00:00Z" });
  beforeEach(() => {
    mocks.listServerFiles.mockImplementation((_name: string, path: string) =>
      Promise.resolve({
        path,
        truncated: false,
        entries:
          path === "config"
            ? [entry("paper-global.yml"), entry("bukkit.yml")]
            : [entry("server.properties"), entry("Pack.ZIP"), entry("old.zip", true), entry("config", true), entry("world", true)],
      }),
    );
  });

  const conflicted = (over: Partial<FileOp> = {}) =>
    fileOp({
      id: "u1",
      path: "Pack.ZIP",
      state: "failed",
      error: { code: "file_exists", message: "", conflicts: ["world/level.dat", "ops.json"], conflict_count: 3 },
      ...over,
    });
  const opsBanner = () => screen.getByRole("region", { name: t("files:ops_label") });
  const poll = () => act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));

  it("extracts an archive, lists what it would replace, and replaces them once confirmed", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderFiles();
    await screen.findByText("Pack.ZIP");
    expect(screen.queryByRole("button", { name: i18next.t("files:unzip_item", { name: "server.properties" }) })).toBeNull();
    expect(screen.queryByRole("button", { name: i18next.t("files:unzip_item", { name: "old.zip" }) })).toBeNull();
    const first = fileOp({ id: "u1", path: "Pack.ZIP" });
    mocks.unzipServerFile.mockResolvedValueOnce({ op: first });
    opsNow = [first];

    fireEvent.click(button("unzip_item", "Pack.ZIP"));

    const banner = await screen.findByRole("region", { name: t("files:ops_label") });
    expect(within(banner).getByText(i18next.t("files:op_unzip", { path: "Pack.ZIP" }))).toBeTruthy();
    expect(mocks.unzipServerFile.mock.calls).toEqual([["lobby", "Pack.ZIP", false]]);
    expect(button("unzip_item", "Pack.ZIP").disabled).toBe(true);
    expect(button("unzip_item", "Pack.ZIP").title).toBe(t("files:wait_for_op"));

    opsNow = [conflicted()];
    await poll();

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(i18next.t("files:unzip_conflicts_title", { name: "Pack.ZIP", count: 3 }))).toBeTruthy();
    expect(within(dialog).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["world/level.dat", "ops.json"]);
    expect(within(dialog).getByText(i18next.t("files:unzip_conflicts_more", { count: 1 }))).toBeTruthy();
    const lists = mocks.listServerFiles.mock.calls.length;

    const second = fileOp({ id: "u2", path: "Pack.ZIP" });
    mocks.unzipServerFile.mockResolvedValueOnce({ op: second });
    opsNow = [second, conflicted()];
    fireEvent.click(within(dialog).getByRole("button", { name: t("files:unzip_overwrite") }));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(mocks.unzipServerFile.mock.calls).toEqual([
      ["lobby", "Pack.ZIP", false],
      ["lobby", "Pack.ZIP", true],
    ]);
    // The refused try is dismissed: the one replacing takes its place.
    expect(within(opsBanner()).getAllByRole("listitem")).toHaveLength(1);
    expect(within(opsBanner()).getByText(t("files:op_preparing"))).toBeTruthy();

    opsNow = [fileOp({ id: "u2", path: "Pack.ZIP", state: "succeeded", files: 5, bytes: 1024 }), conflicted()];
    await poll();

    expect(within(opsBanner()).getByText(i18next.t("files:op_unzip_done", { count: 5, bytes: "1.0 KiB" }))).toBeTruthy();
    await waitFor(() => expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 1));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("holds every change while an extraction starts, and asks at once when it ended before a read", async () => {
    let answer!: (v: { op: FileOp }) => void;
    mocks.unzipServerFile.mockReturnValueOnce(new Promise((res) => (answer = res)));
    renderFiles();
    await screen.findByText("Pack.ZIP");

    fireEvent.click(button("unzip_item", "Pack.ZIP"));

    for (const b of [button("new_file"), button("download_item", "server.properties"), button("delete_item", "world")]) {
      expect(b.disabled).toBe(true);
      expect(b.title).toBe(t("files:wait_for_op"));
    }
    mocks.uploadServerFile.mockResolvedValue({ path: "a.jar", status: "uploaded", sha256: "a", size: 5 });
    fireEvent.change(screen.getByTestId("upload-input"), { target: { files: [new File(["bytes"], "a.jar")] } });
    await act(async () => {});
    expect(mocks.uploadServerFile).not.toHaveBeenCalled();

    await act(async () => answer({ op: conflicted() }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(i18next.t("files:unzip_conflicts_title", { name: "Pack.ZIP", count: 3 }))).toBeTruthy();
    await waitFor(() => expect(mocks.uploadServerFile.mock.calls.map((c) => c[1])).toEqual(["a.jar"]));
  });

  it("asks nothing about an extraction started here that failed for another reason", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const started = fileOp({ id: "u1", path: "Pack.ZIP" });
    mocks.unzipServerFile.mockResolvedValueOnce({ op: started });
    renderFiles();
    await screen.findByText("Pack.ZIP");
    opsNow = [started];
    fireEvent.click(button("unzip_item", "Pack.ZIP"));
    await screen.findByRole("region", { name: t("files:ops_label") });
    expect(mocks.unzipServerFile.mock.calls).toEqual([["lobby", "Pack.ZIP", false]]);

    opsNow = [conflicted({ error: { code: "archive_invalid", message: "zip: not a valid zip file" } })];
    await poll();

    expect(within(opsBanner()).getByText(t("files:archive_invalid"))).toBeTruthy();
    expect(within(opsBanner()).queryByRole("button", { name: t("files:op_conflicts") })).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("asks nothing at once about an extraction started elsewhere, and offers its conflicts", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    opsNow = [fileOp({ id: "u9", path: "maps/Pack.ZIP" })];
    renderFiles();
    await within(await screen.findByRole("region", { name: t("files:ops_label") })).findByText(t("files:op_preparing"));

    opsNow = [conflicted({ id: "u9", path: "maps/Pack.ZIP", error: { code: "file_exists", message: "", conflicts: ["a.txt"] } })];
    await poll();

    expect(within(opsBanner()).getByText(i18next.t("files:op_unzip_conflicts", { count: 1 }))).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(within(opsBanner()).getByRole("button", { name: t("files:op_conflicts") }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(i18next.t("files:unzip_conflicts_title", { name: "Pack.ZIP", count: 1 }))).toBeTruthy();
    expect(within(dialog).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["a.txt"]);
    expect(within(dialog).queryByText(i18next.t("files:unzip_conflicts_more", { count: 0 }))).toBeNull();
    mocks.unzipServerFile.mockResolvedValueOnce({ op: fileOp({ id: "u10", path: "maps/Pack.ZIP" }) });

    fireEvent.click(within(dialog).getByRole("button", { name: t("files:unzip_overwrite") }));

    await waitFor(() => expect(mocks.unzipServerFile.mock.calls).toEqual([["lobby", "maps/Pack.ZIP", true]]));
  });

  it("says why an extraction could not start, and lets it be tried again", async () => {
    const held = { status: 409, code: "maintenance_in_progress", message: "a backup holds the world" };
    mocks.unzipServerFile.mockRejectedValueOnce(held);
    renderFiles();
    await screen.findByText("Pack.ZIP");

    fireEvent.click(button("unzip_item", "Pack.ZIP"));

    expect(await screen.findByText(humanizeError(held))).toBeTruthy();
    expect(button("unzip_item", "Pack.ZIP").disabled).toBe(false);
    expect(screen.queryByRole("region", { name: t("files:ops_label") })).toBeNull();
  });

  it("says when the background ops cannot be read, until a refresh gets through", async () => {
    const broke = { status: 500, code: "internal", message: "" };
    mocks.listServerFileOps.mockRejectedValueOnce(broke);
    renderFiles();

    const ops = await screen.findByRole("region", { name: t("files:ops_label") });
    expect(within(ops).getByText(i18next.t("files:ops_refresh_failed", { reason: humanizeError(broke) }))).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: t("files:refresh") }));

    await waitFor(() => expect(screen.queryByRole("region", { name: t("files:ops_label") })).toBeNull());
  });

  function watchLinks() {
    const clicked: { href: string | null; download: string; hidden: boolean; connected: boolean }[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push({ href: this.getAttribute("href"), download: this.download, hidden: this.hidden, connected: this.isConnected });
    });
    return clicked;
  }
  // readyAfter answers "pending" for the first n reads of the ticket, then "ready".
  function readyAfter(n: number) {
    let reads = 0;
    mocks.exportStatus.mockImplementation(async () => ({ state: reads++ < n ? "pending" : "ready" }));
  }
  const exportPoll = () => act(() => vi.advanceTimersByTimeAsync(EXPORT_POLL_MS));

  it("downloads files after StrictMode replays effects", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const clicked = watchLinks();
    mocks.downloadServerFile.mockResolvedValue({ ticket: "t1", state: "pending", filename: "server.properties" });
    mocks.exportDownloadURL.mockResolvedValue("/api/v1/exports/t1/download");
    readyAfter(0);
    renderFiles(true);
    await userEvent.click(await screen.findByRole("button", { name: i18next.t("files:download_item", { name: "server.properties" }) }));
    await exportPoll();
    await waitFor(() => expect(clicked).toEqual([{ href: "/api/v1/exports/t1/download", download: "server.properties", hidden: true, connected: true }]));
  });

  it("hands a file to the browser once it is ready, holding every change while it is prepared", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const clicked = watchLinks();
    mocks.downloadServerFile.mockResolvedValue({ ticket: "t1", state: "pending", filename: "server.properties" });
    mocks.exportDownloadURL.mockResolvedValue("/api/v1/exports/t1/download");
    mocks.uploadServerFile.mockResolvedValue({ path: "a.jar", status: "uploaded", sha256: "a", size: 5 });
    readyAfter(1);
    renderFiles();
    await screen.findByText("Pack.ZIP");

    fireEvent.click(button("download_item", "server.properties"));

    await waitFor(() => expect(mocks.downloadServerFile.mock.calls).toEqual([["lobby", "server.properties", false]]));
    expect(button("download_item", "server.properties").title).toBe(
      i18next.t("files:download_preparing", { name: "server.properties" }),
    );
    for (const b of [button("new_file"), button("download_folder_item", "world"), button("delete_item", "world")]) {
      expect(b.disabled).toBe(true);
      expect(b.title).toBe(t("files:wait_for_download"));
    }
    fireEvent.change(screen.getByTestId("upload-input"), { target: { files: [new File(["bytes"], "a.jar")] } });
    await exportPoll();
    expect(mocks.exportStatus.mock.calls).toEqual([["t1"]]);
    expect(clicked).toEqual([]);
    expect(mocks.uploadServerFile).not.toHaveBeenCalled();

    await exportPoll();

    expect(clicked).toEqual([{ href: "/api/v1/exports/t1/download", download: "server.properties", hidden: true, connected: true }]);
    expect(await screen.findByText(i18next.t("files:download_started_props", { filename: "server.properties" }))).toBeTruthy();
    expect(button("new_file").disabled).toBe(false);
    await waitFor(() => expect(mocks.uploadServerFile.mock.calls.map((c) => c[1])).toEqual(["a.jar"]));
  });

  it("abandons a download the page left before it was ready", async () => {
    const clicked = watchLinks();
    mocks.downloadServerFile.mockResolvedValue({ ticket: "t1", state: "pending", filename: "world.zip" });
    readyAfter(0);
    const { unmount } = renderFiles();
    await screen.findByText("Pack.ZIP");
    vi.useFakeTimers();
    await act(async () => {
      fireEvent.click(button("download_folder_item", "world"));
    });

    unmount();
    await exportPoll();

    expect(mocks.exportStatus).not.toHaveBeenCalled();
    expect(clicked).toEqual([]);
  });

  it.each([
    ["config", "download_started_config"],
    ["world", "download_started"],
  ])("downloads the folder %s as a zip, and says what it leaves out", async (folder, note) => {
    watchLinks();
    mocks.downloadServerFile.mockResolvedValue({ ticket: "t1", state: "pending", filename: `lobby-${folder}.zip` });
    mocks.exportDownloadURL.mockResolvedValue("/api/v1/exports/t1/download");
    readyAfter(0);
    renderFiles();
    await screen.findByText("Pack.ZIP");
    vi.useFakeTimers();

    await act(async () => {
      fireEvent.click(button("download_folder_item", folder));
    });
    await exportPoll();

    expect(mocks.downloadServerFile.mock.calls).toEqual([["lobby", folder, true]]);
    expect(screen.getByText(i18next.t(`files:${note}`, { filename: `lobby-${folder}.zip` }))).toBeTruthy();
  });

  it("offers no download of the proxy secret, and one of the files beside it", async () => {
    renderFiles();
    fireEvent.click(await screen.findByRole("button", { name: i18next.t("files:open_folder", { name: "config" }) }));
    await screen.findByText("bukkit.yml");

    expect(button("download_item", "paper-global.yml").disabled).toBe(true);
    expect(button("download_item", "paper-global.yml").title).toBe(t("files:secret_config_no_download"));
    expect(button("download_item", "bukkit.yml").disabled).toBe(false);
  });

  it.each([
    [
      "refused as too many",
      () => mocks.downloadServerFile.mockRejectedValue({ status: 429, code: "export_busy", message: "one at a time" }),
      () => t("files:download_busy"),
    ],
    [
      "refused otherwise",
      () => mocks.downloadServerFile.mockRejectedValue({ status: 409, code: "maintenance_in_progress", message: "a backup holds the world" }),
      () =>
        i18next.t("files:download_failed_because", {
          reason: humanizeError({ status: 409, code: "maintenance_in_progress", message: "a backup holds the world" }),
        }),
    ],
    [
      "failed with a reason",
      () => mocks.exportStatus.mockResolvedValue({ state: "failed", message: "tar: world: Cannot open" }),
      () => i18next.t("files:download_failed_because", { reason: "tar: world: Cannot open" }),
    ],
    ["failed without one", () => mocks.exportStatus.mockResolvedValue({ state: "failed" }), () => t("files:download_failed")],
    [
      "killed for memory",
      () =>
        mocks.exportStatus.mockResolvedValue({
          state: "failed",
          message: "the job ran out of memory and the system stopped it (OOMKilled)",
        }),
      () => t("files:download_out_of_memory"),
    ],
  ])("says why a download could not be prepared when %s", async (_label, arrange, words) => {
    const clicked = watchLinks();
    mocks.downloadServerFile.mockResolvedValue({ ticket: "t1", state: "pending", filename: "world.zip" });
    arrange();
    renderFiles();
    await screen.findByText("Pack.ZIP");
    vi.useFakeTimers();

    await act(async () => {
      fireEvent.click(button("download_folder_item", "world"));
    });
    await exportPoll();

    expect(screen.getByText(words())).toBeTruthy();
    expect(clicked).toEqual([]);
    expect(mocks.exportDownloadURL).not.toHaveBeenCalled();
    expect(button("download_folder_item", "world").disabled).toBe(false);
  });
});

describe("ServerFiles with the world held elsewhere", () => {
  const job = (over: Partial<ServerJob>): ServerJob => ({ name: "j1", kind: "backup", state: "running", ...over });
  const poll = () => act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));
  // The notice sits in a status region; other messages can be status regions too.
  const notice = (key: string) => screen.queryByText(t(key));
  const shown = async (key: string) => (await screen.findByText(t(key))).closest('[role="status"]') !== null;

  it("holds every change while a backup runs, says so, and lets go once it ends", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    jobsNow = [job({ kind: "backup" })];
    mocks.uploadServerFile.mockResolvedValue({ path: "a.jar", status: "uploaded", sha256: "a", size: 5 });
    renderFiles();
    await screen.findByText("server.properties");

    expect(await shown("files:wait_for_backup")).toBe(true);
    for (const b of [button("new_file"), button("new_folder"), button("delete_item", "world")]) {
      expect(b.disabled).toBe(true);
      expect(b.title).toBe(t("files:wait_for_backup"));
    }
    fireEvent.change(screen.getByTestId("upload-input"), { target: { files: [new File(["bytes"], "a.jar")] } });
    await poll();
    expect(mocks.uploadServerFile).not.toHaveBeenCalled();
    const lists = mocks.listServerFiles.mock.calls.length;

    jobsNow = [job({ kind: "backup", state: "succeeded" })];
    await poll();

    await waitFor(() => expect(notice("files:wait_for_backup")).toBeNull());
    expect(button("new_folder").disabled).toBe(false);
    await waitFor(() => expect(mocks.uploadServerFile.mock.calls.map((c) => c[1])).toEqual(["a.jar"]));
    // A backup changed nothing, so only the upload's own landing rereads.
    expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 1);
  });

  it("rereads the folder once a restore ends", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    jobsNow = [job({ kind: "restore" })];
    renderFiles();
    await screen.findByText("server.properties");
    expect(await shown("files:wait_for_restore")).toBe(true);
    const lists = mocks.listServerFiles.mock.calls.length;

    jobsNow = [job({ kind: "restore", state: "succeeded" })];
    await poll();

    await waitFor(() => expect(mocks.listServerFiles.mock.calls.length).toBe(lists + 1));
    expect(notice("files:wait_for_restore")).toBeNull();
  });

  it.each([
    ["a safety snapshot whose restore has yet to start", job({ kind: "backup", state: "succeeded", then_restore: "pending" }), "files:wait_for_restore"],
    ["a world export", job({ kind: "export_world" }), "files:wait_for_world_export"],
    ["a file download", job({ kind: "export_files" }), "files:wait_for_file_download"],
  ])("names %s as what holds it", async (_what, holder, text) => {
    jobsNow = [job({ name: "old", kind: "restore", state: "failed" }), holder];
    renderFiles();
    await screen.findByText("server.properties");

    expect(await shown(text)).toBe(true);
    expect(button("new_file").title).toBe(t(text));
  });

  it("is not held by a backup being downloaded, which reads only the backup store", async () => {
    jobsNow = [job({ kind: "export_backup" })];
    renderFiles();
    await screen.findByText("server.properties");
    await waitFor(() => expect(mocks.serverJobs).toHaveBeenCalled());

    for (const key of ["wait_for_backup", "wait_for_restore", "wait_for_world_export", "wait_for_file_download"]) expect(notice(`files:${key}`)).toBeNull();
    expect(button("new_file").disabled).toBe(false);
  });

  it.each([
    ["an extraction", "unzip_item", mocks.unzipServerFile, humanizeError],
    ["a download", "download_item", mocks.downloadServerFile, (e: unknown) => i18next.t("files:download_failed_because", { reason: humanizeError(e) })],
  ] as const)("looks again when %s is refused because the world is held", async (_what, key, call, said) => {
    mocks.listServerFiles.mockImplementation((_name: string, path: string) =>
      Promise.resolve({ path, truncated: false, entries: [{ name: "pack.zip", size: 8, is_dir: false, mod_time: "2026-09-01T00:00:00Z" }] }),
    );
    const held = { status: 409, code: "maintenance_in_progress", message: "a world export or file download is running" };
    call.mockRejectedValueOnce(held);
    renderFiles();
    await screen.findByText("pack.zip");
    await waitFor(() => expect(mocks.serverJobs).toHaveBeenCalledTimes(1));
    jobsNow = [job({ kind: "export_world" })];

    fireEvent.click(button(key, "pack.zip"));

    expect(await shown("files:wait_for_world_export")).toBe(true);
    expect(screen.getByText(said(held))).toBeTruthy();
    expect(button(key, "pack.zip").disabled).toBe(true);
  });

  it("holds changes while its own download streams to the browser", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    mocks.downloadServerFile.mockResolvedValue({ ticket: "t1", state: "pending", filename: "server.properties" });
    mocks.exportStatus.mockResolvedValue({ state: "ready" });
    mocks.exportDownloadURL.mockResolvedValue("/api/v1/exports/t1/download");
    renderFiles();
    await screen.findByText("server.properties");
    await waitFor(() => expect(mocks.serverJobs).toHaveBeenCalledTimes(1));
    jobsNow = [job({ kind: "export_files" })];

    fireEvent.click(button("download_item", "server.properties"));
    await waitFor(() => expect(mocks.downloadServerFile).toHaveBeenCalledTimes(1));
    expect(notice("files:wait_for_file_download")).toBeNull();
    await act(() => vi.advanceTimersByTimeAsync(EXPORT_POLL_MS));
    expect(mocks.exportDownloadURL).toHaveBeenCalledTimes(1);

    expect(await shown("files:wait_for_file_download")).toBe(true);
    expect(button("new_file").disabled).toBe(true);
  });
});
