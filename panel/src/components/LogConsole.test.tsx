// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import i18next from "i18next";
import { LogConsole } from "./LogConsole";

const tier = vi.hoisted(() => ({ revalidate: () => Promise.resolve() }));
vi.mock("@/lib/tier", () => ({ useTier: () => tier }));

// sameChunk is wrapped so a test can see which chunks React skipped.
const spies = vi.hoisted(() => ({ sameChunk: null as unknown as ReturnType<typeof vi.fn> }));
vi.mock("@/lib/logchunks", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/logchunks")>();
  spies.sameChunk = vi.fn(actual.sameChunk);
  return { ...actual, sameChunk: spies.sameChunk };
});

// FakeEventSource replaces the browser one: jsdom has none.
class FakeEventSource {
  static last: FakeEventSource | null = null;
  onopen: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  readyState = 0;
  constructor() {
    FakeEventSource.last = this;
  }
  close() {
    this.readyState = 2;
  }
  addEventListener() {}
}

// send delivers lines and waits past the frame that commits them.
async function send(...lines: string[]) {
  await act(async () => {
    for (const line of lines) FakeEventSource.last!.onmessage?.({ data: line });
    await new Promise((resolve) => setTimeout(resolve, 40));
  });
}

const range = (from: number, to: number) => Array.from({ length: to - from }, (_, i) => `line ${from + i}`);
const lineRows = (container: HTMLElement) => container.querySelectorAll(".whitespace-pre-wrap");

beforeEach(() => {
  vi.stubGlobal("EventSource", FakeEventSource);
  spies.sameChunk.mockClear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("LogConsole", () => {
  it("renders colour codes as styled text and never as markup", async () => {
    const { container } = render(<LogConsole url="/c" />);
    await send("§c<b>boom</b> §lloud§r plain");

    const red = screen.getByText("<b>boom</b>", { exact: false });
    expect(red.tagName).toBe("SPAN");
    expect(red.style.color).toBe("rgb(255, 85, 85)");
    expect(screen.getByText("loud").style.fontWeight).toBe("700");
    expect(container.querySelector("b")).toBeNull();
    expect(container.textContent).not.toContain("§");
  });

  it("keeps showing new lines in a chunk that was already on screen, and after a clear", async () => {
    const { container } = render(<LogConsole url="/c" />);
    await send(...range(0, 150));
    expect(screen.getByText("line 149")).toBeTruthy();

    await send(...range(150, 160));
    expect(screen.getByText("line 159")).toBeTruthy();
    expect(lineRows(container)).toHaveLength(160);

    fireEvent.click(screen.getByRole("button", { name: i18next.t("servers:log_clear_btn") }));
    await send("after clear");
    expect(screen.getByText("after clear")).toBeTruthy();
    expect(screen.queryByText("line 159")).toBeNull();
    expect(lineRows(container)).toHaveLength(1);
  });

  it("drops the oldest lines from the first chunk as the buffer trims", async () => {
    const { container } = render(<LogConsole url="/c" />);
    await send(...range(0, 2000));
    expect(screen.getByText("line 0")).toBeTruthy();

    await send(...range(2000, 2050));
    expect(screen.queryByText("line 0")).toBeNull();
    expect(screen.queryByText("line 49")).toBeNull();
    expect(screen.getByText("line 50")).toBeTruthy();
    expect(lineRows(container)).toHaveLength(2000);
  });

  it("re-renders only the chunk a new line lands in", async () => {
    render(<LogConsole url="/c" />);
    await send(...range(0, 250));
    spies.sameChunk.mockClear();

    await send("line 250");
    expect(screen.getByText("line 250")).toBeTruthy();
    const verdicts = spies.sameChunk.mock.results.map((r) => r.value);
    expect(verdicts.filter((same) => same === true)).toHaveLength(2);
    expect(verdicts.filter((same) => same === false)).toHaveLength(1);
  });
});

describe("startup log retries", () => {
  it("recovers from a fatal early attach without requiring a manual reconnect", async () => {
    vi.useFakeTimers();
    const view = render(<LogConsole url="/startup" starting />);
    try {
      const original = FakeEventSource.last!;
      act(() => { original.readyState = 2; original.onerror?.({}); });
      expect(screen.getByText(i18next.t("servers:log_reconnecting"))).toBeTruthy();
      expect(screen.getByText(i18next.t("servers:log_starting_wait"))).toBeTruthy();
      await act(async () => vi.advanceTimersByTime(5000));
      expect(FakeEventSource.last).not.toBe(original);
      act(() => FakeEventSource.last!.onopen?.({}));
      expect(screen.getByText(i18next.t("servers:log_live"))).toBeTruthy();
    } finally { view.unmount(); vi.useRealTimers(); }
  });
});
