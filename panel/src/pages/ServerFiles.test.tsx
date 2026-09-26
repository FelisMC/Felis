// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import i18next from "i18next";
import { ServerFiles } from "./ServerFiles";
import { STATUS_POLL_FAST_MS } from "@/lib/hooks";

const mocks = vi.hoisted(() => ({ writeServerFile: vi.fn(), status: vi.fn() }));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      status: mocks.status,
      listServerFiles: () =>
        Promise.resolve({
          path: "",
          truncated: false,
          entries: [{ name: "server.properties", size: 8, is_dir: false, mod_time: "2026-09-01T00:00:00Z" }],
        }),
      readServerFile: () => Promise.resolve({ path: "server.properties", content: btoa("motd=hi\n"), sha256: "abc" }),
      writeServerFile: mocks.writeServerFile,
    },
  };
});
vi.mock("@/lib/tier", () => ({ useTier: () => ({ isAdmin: true, loading: false }) }));

const t = (key: string) => i18next.t(key);

async function openEditor() {
  render(
    <MemoryRouter initialEntries={["/servers/lobby/files"]}>
      <Routes>
        <Route path="/servers/:name/files" element={<ServerFiles />} />
      </Routes>
    </MemoryRouter>,
  );
  await userEvent.click(await screen.findByText("server.properties"));
  const dialog = await screen.findByRole("dialog");
  return { dialog, editor: within(dialog).getByRole("textbox") as HTMLTextAreaElement };
}

const stopped = { name: "lobby", subdomain: "lobby", phase: "Stopped", desiredState: "Stopped", ready: false };

beforeEach(() => {
  mocks.writeServerFile.mockReset();
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
    render(
      <MemoryRouter initialEntries={["/servers/lobby/files"]}>
        <Routes>
          <Route path="/servers/:name/files" element={<ServerFiles />} />
        </Routes>
      </MemoryRouter>,
    );
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
});
