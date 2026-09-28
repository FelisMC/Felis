// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { FileOp } from "@/lib/types";
import { OP_POLL_MS } from "./sessionUpload";
import { useFileOps } from "./useFileOps";

const mocks = vi.hoisted(() => ({ listServerFileOps: vi.fn() }));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...mocks } };
});

function op(id: string, over: Partial<FileOp> = {}): FileOp {
  return { id, op: "unzip", path: `${id}.zip`, state: "running", started_at: "2026-09-28T00:00:00Z", done: 0, total: 0, ...over };
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => (resolve = res));
  return { promise, resolve };
}

const ids = (ops: readonly FileOp[]) => ops.map((o) => `${o.id}:${o.state}`);
const tick = () => act(() => vi.advanceTimersByTimeAsync(OP_POLL_MS));

beforeEach(() => {
  mocks.listServerFileOps.mockReset();
  vi.useFakeTimers();
});
afterEach(() => vi.useRealTimers());

describe("useFileOps", () => {
  it("reads nothing until enabled, then once, and polls no further while nothing runs", async () => {
    mocks.listServerFileOps.mockResolvedValue({ ops: [op("a", { state: "succeeded" })] });
    const { result, rerender } = renderHook(({ on }) => useFileOps("lobby", on, () => {}), { initialProps: { on: false } });
    await tick();
    expect(mocks.listServerFileOps).not.toHaveBeenCalled();

    rerender({ on: true });
    await act(async () => {});
    expect(mocks.listServerFileOps.mock.calls).toEqual([["lobby"]]);
    expect(ids(result.current.ops)).toEqual(["a:succeeded"]);
    expect(result.current.running).toBe(false);

    await tick();
    await tick();
    expect(mocks.listServerFileOps).toHaveBeenCalledTimes(1);
  });

  it("polls while one runs, and tells of each op it saw running once that op ends", async () => {
    const ended = vi.fn();
    mocks.listServerFileOps
      .mockResolvedValueOnce({ ops: [op("a", { done: 1, total: 4 }), op("old", { state: "failed" })] })
      .mockResolvedValueOnce({ ops: [op("a", { done: 3, total: 4 }), op("old", { state: "failed" })] })
      .mockResolvedValue({ ops: [op("a", { state: "succeeded" }), op("old", { state: "failed" })] });
    const { result } = renderHook(() => useFileOps("lobby", true, ended));
    await act(async () => {});
    expect(result.current.running).toBe(true);

    await tick();
    expect(result.current.ops[0].done).toBe(3);
    expect(ended).not.toHaveBeenCalled();

    await tick();
    expect(result.current.running).toBe(false);
    // Only the op seen running here: "old" had ended before the page looked.
    expect(ended.mock.calls.map(([o]) => o.id)).toEqual(["a"]);

    await tick();
    await tick();
    expect(mocks.listServerFileOps).toHaveBeenCalledTimes(3);
    expect(ended).toHaveBeenCalledTimes(1);
  });

  it("shows an op this page started at once, over a read already on its way", async () => {
    const slow = deferred<{ ops: FileOp[] }>();
    mocks.listServerFileOps.mockReturnValueOnce(slow.promise);
    const { result } = renderHook(() => useFileOps("lobby", true, () => {}));

    act(() => result.current.started(op("new")));
    expect(ids(result.current.ops)).toEqual(["new:running"]);
    await act(async () => slow.resolve({ ops: [] }));

    expect(ids(result.current.ops)).toEqual(["new:running"]);
    expect(result.current.running).toBe(true);
  });

  it("tells of the end of an op this page started even when no read saw it running", async () => {
    const ended = vi.fn();
    mocks.listServerFileOps.mockResolvedValueOnce({ ops: [] }).mockResolvedValue({ ops: [op("new", { state: "failed" })] });
    const { result } = renderHook(() => useFileOps("lobby", true, ended));
    await act(async () => {});

    act(() => result.current.started(op("new")));
    await tick();

    expect(ended.mock.calls.map(([o]) => `${o.id}:${o.state}`)).toEqual(["new:failed"]);
  });

  it("starts no read over one still on its way", async () => {
    const slow = deferred<{ ops: FileOp[] }>();
    mocks.listServerFileOps.mockResolvedValueOnce({ ops: [op("a")] }).mockReturnValueOnce(slow.promise);
    renderHook(() => useFileOps("lobby", true, () => {}));
    await act(async () => {});

    await tick();
    await tick();
    await tick();

    expect(mocks.listServerFileOps).toHaveBeenCalledTimes(2);
  });

  it("keeps an ignored op out from then on, and never tells of its end", async () => {
    const ended = vi.fn();
    mocks.listServerFileOps
      .mockResolvedValueOnce({ ops: [op("mine", { op: "upload" }), op("theirs")] })
      .mockResolvedValue({ ops: [op("mine", { op: "upload", state: "succeeded" }), op("theirs", { state: "succeeded" })] });
    const { result } = renderHook(() => useFileOps("lobby", true, ended));
    await act(async () => {});

    act(() => result.current.ignore("mine"));
    expect(ids(result.current.ops)).toEqual(["theirs:running"]);
    await tick();

    expect(ids(result.current.ops)).toEqual(["theirs:succeeded"]);
    expect(ended.mock.calls.map(([o]) => o.id)).toEqual(["theirs"]);
  });

  it("an ignored op that runs alone holds nothing", async () => {
    mocks.listServerFileOps.mockResolvedValue({ ops: [op("mine", { op: "upload" })] });
    const { result } = renderHook(() => useFileOps("lobby", true, () => {}));
    await act(async () => {});
    expect(result.current.running).toBe(true);

    act(() => result.current.ignore("mine"));

    expect(result.current.running).toBe(false);
  });

  it("hides a dismissed op", async () => {
    mocks.listServerFileOps.mockResolvedValue({ ops: [op("a", { state: "failed" }), op("b", { state: "succeeded" })] });
    const { result } = renderHook(() => useFileOps("lobby", true, () => {}));
    await act(async () => {});

    act(() => result.current.dismiss("a"));

    expect(ids(result.current.ops)).toEqual(["b:succeeded"]);
  });

  it("says why a read failed, and clears it once one gets through", async () => {
    const refused = { status: 500, code: "internal", message: "" };
    mocks.listServerFileOps.mockRejectedValueOnce(refused).mockResolvedValueOnce({ ops: [] });
    const { result } = renderHook(() => useFileOps("lobby", true, () => {}));
    await act(async () => {});
    expect(result.current.error).toBe(refused);

    await act(async () => result.current.refresh());

    expect(result.current.error).toBeNull();
  });
});
