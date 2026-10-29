// @vitest-environment jsdom
import { createHash } from "node:crypto";
import { StrictMode } from "react";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import i18next from "i18next";
import { ConsoleHistory } from "./ConsoleHistory";
import { humanizeError } from "@/lib/api";
import type { ConsoleHistoryPage } from "@/lib/types";

const calls = vi.hoisted(() => ({ history: vi.fn() }));
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, serverConsoleHistory: calls.history } };
});

const t = (key: string) => i18next.t(`servers:${key}`);
function page(content: string | Uint8Array, next_cursor?: string, byte_limited = false): ConsoleHistoryPage {
  const bytes = typeof content === "string" ? new TextEncoder().encode(content) : content;
  return { content: Buffer.from(bytes).toString("base64"), content_sha256: createHash("sha256").update(bytes).digest("hex"), has_more: !!next_cursor, byte_limited, ...(next_cursor ? { next_cursor } : {}) };
}
const rows = () => Array.from(screen.getByRole("region", { name: t("log_history_content") }).querySelectorAll(".whitespace-pre-wrap"));

beforeEach(() => { calls.history.mockReset(); });

describe("saved console history", () => {
  it("shows a read-only formatted log and loads earlier bytes in order without an extra trailing line", async () => {
    calls.history.mockResolvedValueOnce(page("[12:00:00 INFO] §c<b>latest</b>\r\n\r\n", "older"))
      .mockResolvedValueOnce(page("first\rsecond\n"));
    const { container } = render(<ConsoleHistory name="lobby" />);
    const line = await screen.findByText("<b>latest</b>", { exact: false });
    expect(line.style.color).toBe("rgb(255, 85, 85)");
    expect(container.querySelector("b")).toBeNull();
    expect(rows()).toHaveLength(2);
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(calls.history).toHaveBeenCalledExactlyOnceWith("lobby", undefined);
    await userEvent.click(screen.getByRole("button", { name: t("log_history_more") }));
    expect(await screen.findByText("first")).toBeTruthy();
    expect(rows().map((row) => row.textContent)).toEqual(["first", "second", "[12:00:00 INFO] <b>latest</b>", "\u00a0"]);
    expect(calls.history).toHaveBeenLastCalledWith("lobby", "older");
    expect(screen.queryByRole("button", { name: t("log_history_more") })).toBeNull();
  });

  it("hides RCON session lines from the saved log until asked, as the live log does", async () => {
    calls.history.mockResolvedValueOnce(page([
      "[03:09:39] [RCON Listener #1/INFO]: Thread RCON Client /10.42.0.62 started",
      "[03:09:39] [RCON Client /10.42.0.62 #53/INFO]: Thread RCON Client /10.42.0.62 shutting down",
      "[03:14:26] [Server thread/INFO]: Saving the game",
      "",
    ].join("\n")));
    render(<ConsoleHistory name="lobby" />);
    expect(await screen.findByText("[03:14:26] [Server thread/INFO]: Saving the game")).toBeTruthy();
    expect(rows()).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: /Show RCON/ }));
    expect(rows()).toHaveLength(3);
  });

  it("joins raw page bytes before decoding a UTF-8 character split across pages", async () => {
    const bytes = Buffer.from("世界\r\nmiddle\nlast\r");
    calls.history.mockResolvedValueOnce(page(bytes.subarray(1), "prefix", true)).mockResolvedValueOnce(page(bytes.subarray(0, 1)));
    render(<ConsoleHistory name="lobby" />);
    expect(await screen.findByText(t("log_history_byte_limit"))).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: t("log_history_more") }));
    expect(await screen.findByText("世界")).toBeTruthy();
    expect(rows().map((row) => row.textContent)).toEqual(["世界", "middle", "last"]);
    expect(screen.getByRole("region", { name: t("log_history_content") }).textContent).not.toContain("\ufffd");
  });

  it("preserves reading position and refuses concurrent earlier-page loads", async () => {
    let resolve!: (value: ConsoleHistoryPage) => void;
    calls.history.mockResolvedValueOnce(page("visible\nlast\n", "older"))
      .mockImplementationOnce(() => new Promise<ConsoleHistoryPage>((done) => { resolve = done; }));
    render(<ConsoleHistory name="lobby" />);
    await screen.findByText("visible");
    const viewport = screen.getByRole("region", { name: t("log_history_content") });
    Object.defineProperty(viewport, "scrollHeight", { configurable: true, get: () => rows().length * 100 });
    viewport.scrollTop = 80;
    const more = screen.getByRole("button", { name: t("log_history_more") });
    fireEvent.click(more);
    fireEvent.click(more);
    expect(more).toHaveProperty("disabled", true);
    expect(calls.history).toHaveBeenCalledTimes(2);
    await act(async () => { resolve(page("older one\nolder two\n")); });
    expect(await screen.findByText("older one")).toBeTruthy();
    expect(viewport.scrollTop).toBe(280);
  });

  it("reports an empty or missing saved log", async () => {
    calls.history.mockResolvedValue(page(""));
    render(<ConsoleHistory name="lobby" />);
    expect(await screen.findByText(t("log_history_empty"))).toBeTruthy();
    expect(rows()).toHaveLength(0);
    expect(screen.queryByRole("button", { name: t("log_history_more") })).toBeNull();
  });

  it("keeps already loaded lines after an error and retries the same earlier cursor", async () => {
    const failure = { code: "files_timeout", status: 504 };
    calls.history.mockResolvedValueOnce(page("retained\n", "older"))
      .mockRejectedValueOnce(failure).mockResolvedValueOnce(page("recovered\n"));
    render(<ConsoleHistory name="lobby" />);
    await screen.findByText("retained");
    await userEvent.click(screen.getByRole("button", { name: t("log_history_more") }));
    expect((await screen.findByRole("alert")).textContent).toContain(humanizeError(failure));
    expect(screen.getByText("retained")).toBeTruthy();
    expect(calls.history).toHaveBeenCalledTimes(2);
    await userEvent.click(screen.getByRole("button", { name: t("log_history_retry") }));
    expect(await screen.findByText("recovered")).toBeTruthy();
    expect(rows().map((row) => row.textContent)).toEqual(["recovered", "retained"]);
    expect(calls.history).toHaveBeenLastCalledWith("lobby", "older");
  });

  it("requires a fresh first page after file_changed and replaces the previous snapshot", async () => {
    calls.history.mockResolvedValueOnce(page("old snapshot\n", "older"))
      .mockRejectedValueOnce({ code: "file_changed", status: 409 }).mockResolvedValueOnce(page("new snapshot\n"));
    render(<ConsoleHistory name="lobby" />);
    await screen.findByText("old snapshot");
    await userEvent.click(screen.getByRole("button", { name: t("log_history_more") }));
    expect((await screen.findByRole("alert")).textContent).toBe(t("log_history_changed"));
    expect(screen.queryByRole("button", { name: t("log_history_more") })).toBeNull();
    expect(screen.queryByRole("button", { name: t("log_history_retry") })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: t("log_history_refresh") }));
    expect(await screen.findByText("new snapshot")).toBeTruthy();
    expect(screen.queryByText("old snapshot")).toBeNull();
    expect(calls.history).toHaveBeenLastCalledWith("lobby", undefined);
  });

  it("retries an initial failure without a cursor", async () => {
    calls.history.mockRejectedValueOnce({ code: "files_unavailable", status: 503 }).mockResolvedValueOnce(page("available\n"));
    render(<ConsoleHistory name="lobby" />);
    await screen.findByRole("alert");
    await userEvent.click(screen.getByRole("button", { name: t("log_history_retry") }));
    expect(await screen.findByText("available")).toBeTruthy();
    expect(calls.history.mock.calls).toEqual([["lobby", undefined], ["lobby", undefined]]);
  });

  it("survives StrictMode effect replay and ignores the older request", async () => {
    let resolve!: (value: ConsoleHistoryPage) => void;
    calls.history.mockImplementationOnce(() => new Promise<ConsoleHistoryPage>((done) => { resolve = done; }))
      .mockResolvedValueOnce(page("current request\n"));
    render(<StrictMode><ConsoleHistory name="lobby" /></StrictMode>);
    expect(await screen.findByText("current request")).toBeTruthy();
    await act(async () => { resolve(page("obsolete request\n")); });
    expect(screen.queryByText("obsolete request")).toBeNull();
    expect(within(screen.getByRole("region", { name: t("log_history_title") })).getByRole("button", { name: t("log_history_refresh") })).toHaveProperty("disabled", false);
  });

  it("ignores a response after the keyed console switches servers", async () => {
    let resolve!: (value: ConsoleHistoryPage) => void;
    calls.history.mockImplementationOnce(() => new Promise<ConsoleHistoryPage>((done) => { resolve = done; }))
      .mockResolvedValueOnce(page("second server\n"));
    const rendered = render(<ConsoleHistory key="first" name="first" />);
    rendered.rerender(<ConsoleHistory key="second" name="second" />);
    expect(await screen.findByText("second server")).toBeTruthy();
    await act(async () => { resolve(page("first server\n")); });
    expect(screen.queryByText("first server")).toBeNull();
    await waitFor(() => expect(calls.history).toHaveBeenLastCalledWith("second", undefined));
  });
});
