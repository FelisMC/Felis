// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { ServerFiles } from "./ServerFiles";
import { humanizeError } from "@/lib/api";
import { MAX_UPLOAD_BYTES } from "@/components/files/useUploads";
import { STATUS_POLL_FAST_MS } from "@/lib/hooks";

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
    },
  };
});
vi.mock("@/lib/tier", () => ({ useTier: () => ({ isAdmin: true, loading: false }) }));

const t = (key: string) => i18next.t(key);

function renderFiles() {
  return render(
    <MemoryRouter initialEntries={["/servers/lobby/files"]}>
      <Routes>
        <Route path="/servers/:name/files" element={<ServerFiles />} />
      </Routes>
    </MemoryRouter>,
  );
}

async function openEditor() {
  renderFiles();
  await userEvent.click(await screen.findByText("server.properties"));
  const dialog = await screen.findByRole("dialog");
  return { dialog, editor: within(dialog).getByRole("textbox") as HTMLTextAreaElement };
}

const stopped = { name: "lobby", subdomain: "lobby", phase: "Stopped", desiredState: "Stopped", ready: false };

beforeEach(() => {
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
  mocks.stop.mockReset();
  mocks.status.mockReset();
  mocks.status.mockResolvedValue(stopped);
});
afterEach(() => {
  vi.useRealTimers();
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

describe("ServerFiles on a running server", () => {
  it("lists the files once the server has stopped, without a reload", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true });
    renderFiles();
    expect(await screen.findByText(t("files:stopped_required_title"))).toBeTruthy();
    expect(screen.queryByText("server.properties")).toBeNull();

    mocks.status.mockResolvedValue(stopped);
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS));
    expect(await screen.findByText("server.properties")).toBeTruthy();
    expect(screen.queryByText(t("files:stopped_required_title"))).toBeNull();

    // Stopped is what it waited for: nothing more to reread.
    const reads = mocks.status.mock.calls.length;
    await act(() => vi.advanceTimersByTimeAsync(STATUS_POLL_FAST_MS * 3));
    expect(mocks.status.mock.calls.length).toBe(reads);
  });

  it("asks before stopping a server with players on it", async () => {
    mocks.stop.mockResolvedValue(undefined);
    mocks.status.mockResolvedValue({ ...stopped, phase: "Running", desiredState: "Running", ready: true, playersOnline: 2 });
    renderFiles();

    await userEvent.click(await screen.findByRole("button", { name: t("servers:stop") }));
    expect(mocks.stop).not.toHaveBeenCalled();
    expect(screen.getByText(i18next.t("servers:stop_confirm_players", { count: 2 }))).toBeTruthy();

    const reads = mocks.status.mock.calls.length;
    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));
    expect(mocks.stop).toHaveBeenCalledWith("lobby");
    // The page rereads at once to follow the stop.
    await waitFor(() => expect(mocks.status.mock.calls.length).toBeGreaterThan(reads));
  });

  it("asks when the player count cannot be read", async () => {
    mocks.status.mockResolvedValue({
      ...stopped,
      phase: "Running",
      desiredState: "Running",
      ready: true,
      playersOnline: 0,
      playerCountUnknown: true,
    });
    renderFiles();

    await userEvent.click(await screen.findByRole("button", { name: t("servers:stop") }));

    expect(mocks.stop).not.toHaveBeenCalled();
    expect(screen.getByText(t("servers:stop_confirm_unknown"))).toBeTruthy();
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

  it("says why the proxy secret's file does not open, and reads nothing", async () => {
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

    await userEvent.click(await screen.findByText("paper-global.yml"));

    expect(screen.getByText(t("files:secret_config_unreadable"))).toBeTruthy();
    expect(mocks.readServerFile).not.toHaveBeenCalled();
    expect(button("rename_item", "paper-global.yml").disabled).toBe(true);
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

  it("refuses a name a folder holds, and a file over the limit, without sending or offering a retry", async () => {
    renderFiles();
    await screen.findByText("world");
    const big = file("world-backup.zip");
    Object.defineProperty(big, "size", { value: MAX_UPLOAD_BYTES + 1 });
    const exact = file("exact.zip");
    Object.defineProperty(exact, "size", { value: MAX_UPLOAD_BYTES });

    pick(file("world"), big, exact);

    expect(await within(queue()).findByText(t("files:upload_folder_there"))).toBeTruthy();
    expect(within(queue()).getByText(i18next.t("files:upload_too_large", { limit: "64 MiB" }))).toBeTruthy();
    expect(within(queue()).queryByRole("button", { name: t("files:upload_retry") })).toBeNull();
    await waitFor(() => expect(sentAs()).toEqual([["exact.zip", false]]));
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

  it("takes dropped files and names the folders it skipped", async () => {
    renderFiles();
    const table = await screen.findByRole("table");
    const dataTransfer = {
      types: ["Files"],
      files: [],
      items: [
        { kind: "file", webkitGetAsEntry: () => ({ isDirectory: true }), getAsFile: () => null },
        { kind: "file", webkitGetAsEntry: () => ({ isDirectory: false }), getAsFile: () => file("a.jar") },
      ],
    };

    fireEvent.drop(table, { dataTransfer });

    await waitFor(() => expect(sentAs()).toEqual([["a.jar", false]]));
    expect(screen.getByText(i18next.t("files:upload_no_folders", { count: 1 }))).toBeTruthy();
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
