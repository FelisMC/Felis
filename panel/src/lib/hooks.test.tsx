// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useAsync, type AsyncOptions } from "./hooks";

// A request the test settles by hand, one per call, in call order.
function requests() {
  const pending: { id: string; resolve: (v: string) => void; reject: (e: unknown) => void }[] = [];
  const fetch = (id: string) =>
    new Promise<string>((resolve, reject) => pending.push({ id, resolve, reject }));
  const settle = async (i: number, how: { ok: string } | { fail: unknown }) => {
    await act(async () => {
      if ("ok" in how) pending[i].resolve(how.ok);
      else pending[i].reject(how.fail);
    });
  };
  return { pending, fetch, settle };
}

// Every render's (id, data, error, loading), so a stale value shown for a
// single render between new deps and the reload cannot slip past.
function mount(opts?: AsyncOptions) {
  const req = requests();
  const seen: { id: string; data: string | null; error: unknown; loading: boolean }[] = [];
  const hook = renderHook(
    ({ id }: { id: string }) => {
      const r = useAsync(() => req.fetch(id), [id], opts);
      seen.push({ id, data: r.data, error: r.error, loading: r.loading });
      return r;
    },
    { initialProps: { id: "a" } },
  );
  return { ...req, ...hook, seen };
}

describe("useAsync", () => {
  it("never shows one id's result while another id loads", async () => {
    const h = mount();
    await h.settle(0, { ok: "server a" });
    expect(h.result.current.data).toBe("server a");

    h.rerender({ id: "b" });
    const forB = h.seen.filter((s) => s.id === "b");
    expect(forB.length).toBeGreaterThan(0);
    expect(forB.filter((s) => s.data !== null || !s.loading)).toEqual([]);

    await h.settle(1, { ok: "server b" });
    expect(h.result.current).toMatchObject({ data: "server b", loading: false });
  });

  it("never shows one id's error for another id", async () => {
    const h = mount();
    await h.settle(0, { fail: new Error("a is gone") });
    expect(h.result.current.error).toBeInstanceOf(Error);

    h.rerender({ id: "b" });
    const forB = h.seen.filter((s) => s.id === "b");
    expect(forB.length).toBeGreaterThan(0);
    expect(forB.filter((s) => s.error !== null)).toEqual([]);
  });

  it("keeps the data through a reload of the same id", async () => {
    const h = mount();
    await h.settle(0, { ok: "first read" });

    act(() => h.result.current.reload());
    expect(h.result.current).toMatchObject({ data: "first read", loading: true });

    await h.settle(1, { ok: "second read" });
    expect(h.result.current).toMatchObject({ data: "second read", loading: false });
  });

  it("keeps the data and reports the error when a reload of the same id fails", async () => {
    const h = mount();
    await h.settle(0, { ok: "first read" });

    act(() => h.result.current.reload());
    await h.settle(1, { fail: new Error("blip") });
    expect(h.result.current.data).toBe("first read");
    expect(h.result.current.error).toBeInstanceOf(Error);
    expect(h.result.current.loading).toBe(false);
  });

  it("with keepPrevious keeps the last rows until the new ones arrive", async () => {
    const h = mount({ keepPrevious: true });
    await h.settle(0, { ok: "page 1" });

    h.rerender({ id: "b" });
    expect(h.result.current).toMatchObject({ data: "page 1", loading: true });

    await h.settle(1, { ok: "page 2" });
    expect(h.result.current).toMatchObject({ data: "page 2", loading: false });
  });

  it("drops a slow answer for an id it has moved past", async () => {
    const h = mount();
    h.rerender({ id: "b" });
    await h.settle(1, { ok: "server b" });
    await h.settle(0, { ok: "server a" });
    expect(h.result.current.data).toBe("server b");
  });
});
