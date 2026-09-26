// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { ServerFiles } from "./ServerFiles";
import { STATUS_POLL_FAST_MS } from "@/lib/hooks";

const mocks = vi.hoisted(() => ({
  writeServerFile: vi.fn(),
  status: vi.fn(),
  stop: vi.fn(),
  listServerFiles: vi.fn(),
  readServerFile: vi.fn(),
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
    },
  };
});
vi.mock("@/lib/tier", () => ({ useTier: () => ({ isAdmin: true, loading: false }) }));

const t = (key: string) => i18next.t(key);

function renderFiles() {
  render(
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
