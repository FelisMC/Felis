// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import type { FileOp, FileUploadSession } from "@/lib/types";
import { MAX_ATTEMPTS } from "@/lib/contextUpload";
import {
  MAX_OP_MISSES,
  OP_POLL_MS,
  STALE_SESSION_MS,
  discardSession,
  forgetSession,
  sendInParts,
  watchOp,
} from "./sessionUpload";

const mocks = vi.hoisted(() => ({
  getServerFileUpload: vi.fn(),
  beginServerFileUpload: vi.fn(),
  putServerFileUploadPart: vi.fn(),
  deleteServerFileUpload: vi.fn(),
  commitServerFileUpload: vi.fn(),
  listServerFileOps: vi.fn(),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...mocks } };
});

const KEY = "felis-file-upload:lobby:world.zip";
const PART = 4;
const NOW = 1_000_000_000;

// A 10-byte file sent in parts of 4: offsets 0, 4 and 8.
function file(body = "0123456789", modified = 111) {
  return new File([body], "world.zip", { lastModified: modified });
}

function session(received: number, over: Partial<FileUploadSession> = {}): FileUploadSession {
  return { id: "s1", path: "world.zip", size: 10, received, part_max_bytes: PART, ...over };
}

function op(over: Partial<FileOp> = {}): FileOp {
  return {
    id: "op1",
    op: "upload",
    path: "world.zip",
    state: "running",
    started_at: "2026-09-28T00:00:00Z",
    done: 0,
    total: 0,
    ...over,
  };
}

const netErr = { status: 0, code: "network_error", message: "" };
const gone = { status: 404, code: "upload_not_found", message: "no such upload" };
const held = { status: 409, code: "maintenance_in_progress", message: "held" };

// The server takes every part whole and reports where the session stands.
function serverTakesParts() {
  mocks.putServerFileUploadPart.mockImplementation(async (_n: string, id: string, offset: number, part: Blob) =>
    session(offset + part.size, { id }),
  );
}

// Pauses at once. A loop that never gives up would never yield either, so
// the hundredth pause fails the test instead of hanging it.
const sleep = vi.fn(async (_ms: number, _signal?: AbortSignal) => {
  if (sleep.mock.calls.length > 100) throw new Error("runaway retry loop");
});
const opts = (over: Partial<Parameters<typeof sendInParts>[3]> = {}) => ({
  overwrite: false,
  sleep,
  now: () => NOW,
  ...over,
});
const offsets = () => mocks.putServerFileUploadPart.mock.calls.map((c) => c[2]);
const stored = () => JSON.parse(localStorage.getItem(KEY) ?? "null");

beforeEach(() => {
  for (const m of Object.values(mocks)) m.mockReset();
  sleep.mockClear();
  localStorage.clear();
  mocks.beginServerFileUpload.mockResolvedValue(session(0));
  mocks.commitServerFileUpload.mockResolvedValue({ op: op() });
  serverTakesParts();
});

describe("sendInParts", () => {
  it("sends each part at the offset the server has reached, then commits", async () => {
    const seen: number[] = [];
    const sessions: string[] = [];

    const got = await sendInParts("lobby", "world.zip", file(), opts({
      overwrite: true,
      onProgress: (n) => seen.push(n),
      onSession: (id) => sessions.push(id),
    }));

    expect(got).toEqual(op());
    expect(mocks.beginServerFileUpload.mock.calls).toEqual([["lobby", "world.zip", 10]]);
    expect(offsets()).toEqual([0, 4, 8]);
    expect(mocks.putServerFileUploadPart.mock.calls.map((c) => (c[3] as Blob).size)).toEqual([4, 4, 2]);
    expect(mocks.commitServerFileUpload.mock.calls).toEqual([["lobby", "s1", true]]);
    expect(seen).toEqual([0, 4, 8, 10]);
    expect(sessions).toEqual(["s1"]);
  });

  it("remembers the session under the server and path, with the file it holds, touched at each part", async () => {
    const f = file("0123456789", 777);
    let clock = NOW;
    let seenMidway: unknown = null;
    mocks.putServerFileUploadPart.mockImplementation(async (_n: string, id: string, offset: number, part: Blob) => {
      if (offset === 4) seenMidway = stored();
      clock += 1000;
      return session(offset + part.size, { id });
    });

    await sendInParts("lobby", "world.zip", f, opts({ now: () => clock }));

    expect(seenMidway).toEqual({ server: "lobby", path: "world.zip", id: "s1", size: 10, modified: 777, touched: NOW + 1000 });
    expect(stored().touched).toBe(NOW + 3000);
  });

  it("carries on a remembered session from where the server says it stands", async () => {
    localStorage.setItem(KEY, JSON.stringify({ server: "lobby", path: "world.zip", id: "s7", size: 10, modified: 111, touched: 0 }));
    mocks.getServerFileUpload.mockResolvedValue(session(8, { id: "s7" }));

    await sendInParts("lobby", "world.zip", file(), opts());

    expect(mocks.getServerFileUpload.mock.calls).toEqual([["lobby", "s7"]]);
    expect(mocks.beginServerFileUpload).not.toHaveBeenCalled();
    expect(offsets()).toEqual([8]);
    expect(mocks.commitServerFileUpload.mock.calls).toEqual([["lobby", "s7", false]]);
  });

  it("commits at once when the server already holds the whole file", async () => {
    localStorage.setItem(KEY, JSON.stringify({ server: "lobby", path: "world.zip", id: "s7", size: 10, modified: 111, touched: 0 }));
    mocks.getServerFileUpload.mockResolvedValue(session(10, { id: "s7" }));

    await sendInParts("lobby", "world.zip", file(), opts());

    expect(offsets()).toEqual([]);
    expect(mocks.commitServerFileUpload.mock.calls).toEqual([["lobby", "s7", false]]);
  });

  it.each([
    ["a different size", { size: 11, modified: 111 }],
    ["a different modification time", { size: 10, modified: 112 }],
  ])("starts over when the remembered session was for %s", async (_label, held) => {
    localStorage.setItem(KEY, JSON.stringify({ server: "lobby", path: "world.zip", id: "s7", touched: 0, ...held }));

    await sendInParts("lobby", "world.zip", file(), opts());

    expect(mocks.getServerFileUpload).not.toHaveBeenCalled();
    expect(mocks.beginServerFileUpload).toHaveBeenCalledTimes(1);
    expect(offsets()).toEqual([0, 4, 8]);
  });

  it.each([
    ["is gone", () => mocks.getServerFileUpload.mockRejectedValue(gone)],
    ["now stands for another file", () => mocks.getServerFileUpload.mockResolvedValue(session(8, { id: "s7", size: 99 }))],
  ])("begins a new session when the remembered one %s", async (_label, arrange) => {
    localStorage.setItem(KEY, JSON.stringify({ server: "lobby", path: "world.zip", id: "s7", size: 10, modified: 111, touched: 0 }));
    arrange();

    await sendInParts("lobby", "world.zip", file(), opts());

    expect(mocks.beginServerFileUpload).toHaveBeenCalledTimes(1);
    expect(offsets()).toEqual([0, 4, 8]);
    expect(stored().id).toBe("s1");
    expect(sleep).not.toHaveBeenCalled();
  });

  it("gives back only the stale sessions this browser left behind when there are too many, then begins", async () => {
    const old = { server: "lobby", path: "old.zip", id: "old", size: 5, modified: 1, touched: NOW - STALE_SESSION_MS };
    const fresh = { server: "lobby", path: "fresh.zip", id: "fresh", size: 5, modified: 1, touched: NOW - STALE_SESSION_MS + 1 };
    localStorage.setItem("felis-file-upload:lobby:old.zip", JSON.stringify(old));
    localStorage.setItem("felis-file-upload:lobby:fresh.zip", JSON.stringify(fresh));
    localStorage.setItem("unrelated", JSON.stringify({ id: "x", server: "lobby", path: "x" }));
    mocks.beginServerFileUpload
      .mockRejectedValueOnce({ status: 429, code: "too_many_uploads", message: "" })
      .mockResolvedValueOnce(session(0));
    mocks.deleteServerFileUpload.mockResolvedValue(null);

    await sendInParts("lobby", "world.zip", file(), opts());

    expect(mocks.deleteServerFileUpload.mock.calls).toEqual([["lobby", "old"]]);
    expect(localStorage.getItem("felis-file-upload:lobby:old.zip")).toBeNull();
    expect(localStorage.getItem("felis-file-upload:lobby:fresh.zip")).not.toBeNull();
    expect(mocks.beginServerFileUpload).toHaveBeenCalledTimes(2);
    expect(offsets()).toEqual([0, 4, 8]);
  });

  it("reports too many uploads when none of this browser's sessions could be given back", async () => {
    const refusal = { status: 429, code: "too_many_uploads", message: "" };
    const old = { server: "lobby", path: "old.zip", id: "old", size: 5, modified: 1, touched: 0 };
    localStorage.setItem("felis-file-upload:lobby:old.zip", JSON.stringify(old));
    mocks.beginServerFileUpload.mockRejectedValue(refusal);
    mocks.deleteServerFileUpload.mockRejectedValue(gone);

    await expect(sendInParts("lobby", "world.zip", file(), opts())).rejects.toBe(refusal);

    expect(mocks.beginServerFileUpload).toHaveBeenCalledTimes(1);
    expect(offsets()).toEqual([]);
  });

  it("asks where the session stands after a dropped part, and resends from there", async () => {
    let first = true;
    mocks.putServerFileUploadPart.mockImplementation(async (_n: string, id: string, offset: number, part: Blob) => {
      if (offset === 4 && first) {
        first = false;
        throw netErr;
      }
      return session(offset + part.size, { id });
    });
    // Half of the dropped part had arrived.
    mocks.getServerFileUpload.mockResolvedValue(session(6));

    await sendInParts("lobby", "world.zip", file(), opts());

    expect(mocks.getServerFileUpload.mock.calls).toEqual([["lobby", "s1"]]);
    expect(offsets()).toEqual([0, 4, 6]);
    expect(sleep.mock.calls.map((c) => c[0])).toEqual([1000]);
  });

  it("begins a new session when the one in use went away mid-upload", async () => {
    mocks.putServerFileUploadPart
      .mockImplementationOnce(async (_n: string, id: string, offset: number, part: Blob) => session(offset + part.size, { id }))
      .mockRejectedValueOnce(gone);
    mocks.beginServerFileUpload.mockResolvedValueOnce(session(0)).mockResolvedValueOnce(session(0, { id: "s2" }));

    await sendInParts("lobby", "world.zip", file(), opts());

    expect(mocks.getServerFileUpload).not.toHaveBeenCalled();
    expect(mocks.beginServerFileUpload).toHaveBeenCalledTimes(2);
    expect(offsets()).toEqual([0, 4, 0, 4, 8]);
    expect(mocks.commitServerFileUpload.mock.calls).toEqual([["lobby", "s2", false]]);
  });

  it("stops at a refusal sending again cannot change", async () => {
    const full = { status: 507, code: "upload_staging_full", message: "" };
    mocks.putServerFileUploadPart.mockRejectedValue(full);

    await expect(sendInParts("lobby", "world.zip", file(), opts())).rejects.toBe(full);

    expect(offsets()).toEqual([0]);
    expect(sleep).not.toHaveBeenCalled();
  });

  it(`gives up after ${MAX_ATTEMPTS} dropped attempts in a row`, async () => {
    mocks.putServerFileUploadPart.mockRejectedValue(netErr);
    mocks.getServerFileUpload.mockResolvedValue(session(0));

    await expect(sendInParts("lobby", "world.zip", file(), opts())).rejects.toBe(netErr);

    expect(offsets()).toHaveLength(MAX_ATTEMPTS);
  });

  it("counts only drops in a row", async () => {
    let received = 0;
    let drops = 0;
    mocks.putServerFileUploadPart.mockImplementation(async (_n: string, id: string, offset: number, part: Blob) => {
      // Each part lands only after MAX_ATTEMPTS - 1 drops.
      if (drops++ < MAX_ATTEMPTS - 1) throw netErr;
      drops = 0;
      received = offset + part.size;
      return session(received, { id });
    });
    mocks.getServerFileUpload.mockImplementation(async () => session(received));

    await sendInParts("lobby", "world.zip", file(), opts());

    expect(offsets()).toHaveLength(3 * MAX_ATTEMPTS);
    expect(mocks.commitServerFileUpload).toHaveBeenCalledTimes(1);
  });

  it("keeps the session remembered when stopped, even in its first part, for a later resume", async () => {
    const ctrl = new AbortController();
    mocks.putServerFileUploadPart.mockImplementation(async (_n: string, id: string, offset: number, part: Blob) => {
      if (offset === 0) {
        ctrl.abort();
        throw new DOMException("cancelled", "AbortError");
      }
      return session(offset + part.size, { id });
    });

    await expect(sendInParts("lobby", "world.zip", file(), opts({ signal: ctrl.signal }))).rejects.toMatchObject({
      name: "AbortError",
    });

    expect(mocks.putServerFileUploadPart.mock.calls[0][4]).toMatchObject({ signal: ctrl.signal });
    expect(stored().id).toBe("s1");
    expect(mocks.deleteServerFileUpload).not.toHaveBeenCalled();
  });

  describe("a commit whose answer was lost", () => {
    it("takes the running upload of this path as its answer when the world is held", async () => {
      const landing = op({ id: "op9" });
      mocks.commitServerFileUpload.mockRejectedValueOnce(netErr).mockRejectedValueOnce(held);
      mocks.listServerFileOps.mockResolvedValue({
        ops: [op({ id: "other", path: "else.zip" }), op({ id: "old", state: "failed" }), landing],
      });

      expect(await sendInParts("lobby", "world.zip", file(), opts())).toEqual(landing);
      expect(mocks.commitServerFileUpload).toHaveBeenCalledTimes(2);
    });

    it("takes the upload of this path in any state when the session is already gone", async () => {
      const landed = op({ id: "op9", state: "succeeded" });
      mocks.commitServerFileUpload.mockRejectedValueOnce(netErr).mockRejectedValueOnce(gone);
      mocks.listServerFileOps.mockResolvedValue({ ops: [op({ id: "u", op: "unzip" }), landed] });

      expect(await sendInParts("lobby", "world.zip", file(), opts())).toEqual(landed);
    });

    it("reports the session gone when no upload of this path is there", async () => {
      mocks.commitServerFileUpload.mockRejectedValueOnce(netErr).mockRejectedValueOnce(gone);
      mocks.listServerFileOps.mockResolvedValue({ ops: [op({ path: "else.zip" })] });

      await expect(sendInParts("lobby", "world.zip", file(), opts())).rejects.toBe(gone);
    });

    it("asks again while the world is held by something else", async () => {
      mocks.commitServerFileUpload
        .mockRejectedValueOnce(netErr)
        .mockRejectedValueOnce(held)
        .mockResolvedValueOnce({ op: op({ id: "op2" }) });
      mocks.listServerFileOps.mockResolvedValue({ ops: [op({ id: "done", state: "succeeded" })] });

      expect(await sendInParts("lobby", "world.zip", file(), opts())).toEqual(op({ id: "op2" }));
      expect(mocks.commitServerFileUpload).toHaveBeenCalledTimes(3);
    });
  });

  it(`gives up a commit after ${MAX_ATTEMPTS} attempts that did not get through`, async () => {
    mocks.commitServerFileUpload.mockRejectedValue(netErr);

    await expect(sendInParts("lobby", "world.zip", file(), opts())).rejects.toBe(netErr);

    expect(mocks.commitServerFileUpload).toHaveBeenCalledTimes(MAX_ATTEMPTS);
  });

  it("reads a held world on the first commit as the refusal it is", async () => {
    mocks.commitServerFileUpload.mockRejectedValue(held);

    await expect(sendInParts("lobby", "world.zip", file(), opts())).rejects.toBe(held);

    expect(mocks.commitServerFileUpload).toHaveBeenCalledTimes(1);
    expect(mocks.listServerFileOps).not.toHaveBeenCalled();
  });
});

describe("watchOp", () => {
  const watch = (over: Parameters<typeof watchOp>[2] = {}) => watchOp("lobby", "op1", { sleep, ...over });

  it("reads every OP_POLL_MS, reports progress, and answers the op once it ends", async () => {
    const seen: number[] = [];
    mocks.listServerFileOps
      .mockResolvedValueOnce({ ops: [op({ done: 3, total: 10 })] })
      .mockResolvedValueOnce({ ops: [op({ id: "x", state: "succeeded" }), op({ done: 7, total: 10 })] })
      .mockResolvedValueOnce({ ops: [op({ state: "succeeded", done: 10, total: 10 })] });

    const end = await watch({ onProgress: (o) => seen.push(o.done) });

    expect(end).toEqual(op({ state: "succeeded", done: 10, total: 10 }));
    expect(seen).toEqual([3, 7]);
    expect(sleep.mock.calls.map((c) => c[0])).toEqual([OP_POLL_MS, OP_POLL_MS, OP_POLL_MS]);
  });

  it("backs off after a read that did not get through, and carries on", async () => {
    mocks.listServerFileOps
      .mockRejectedValueOnce(netErr)
      .mockRejectedValueOnce(netErr)
      .mockRejectedValueOnce(netErr)
      .mockResolvedValueOnce({ ops: [op()] })
      .mockResolvedValueOnce({ ops: [op({ state: "failed" })] });

    expect((await watch()).state).toBe("failed");
    expect(sleep.mock.calls.map((c) => c[0])).toEqual([OP_POLL_MS, 1000, 2000, 4000, OP_POLL_MS]);
  });

  it(`gives up after ${MAX_ATTEMPTS} reads in a row did not get through`, async () => {
    mocks.listServerFileOps.mockRejectedValue(netErr);

    await expect(watch()).rejects.toBe(netErr);
    expect(mocks.listServerFileOps).toHaveBeenCalledTimes(MAX_ATTEMPTS);
  });

  it("stops at a read refused for good", async () => {
    const refused = { status: 403, code: "forbidden", message: "" };
    mocks.listServerFileOps.mockRejectedValue(refused);

    await expect(watch()).rejects.toBe(refused);
    expect(mocks.listServerFileOps).toHaveBeenCalledTimes(1);
  });

  it(`reports the op lost after ${MAX_OP_MISSES} reads in a row without it`, async () => {
    mocks.listServerFileOps.mockResolvedValue({ ops: [op({ id: "other" })] });

    await expect(watch()).rejects.toMatchObject({ code: "op_lost" });
    expect(mocks.listServerFileOps).toHaveBeenCalledTimes(MAX_OP_MISSES);
  });

  it("counts only misses in a row", async () => {
    const without = { ops: [] };
    for (let i = 0; i < MAX_OP_MISSES - 1; i++) mocks.listServerFileOps.mockResolvedValueOnce(without);
    mocks.listServerFileOps.mockResolvedValueOnce({ ops: [op()] });
    for (let i = 0; i < MAX_OP_MISSES - 1; i++) mocks.listServerFileOps.mockResolvedValueOnce(without);
    mocks.listServerFileOps.mockResolvedValueOnce({ ops: [op({ state: "succeeded" })] });

    expect((await watch()).state).toBe("succeeded");
  });
});

describe("discardSession and forgetSession", () => {
  it("forgets the session and cancels it, waiting out a part still arriving", async () => {
    localStorage.setItem(KEY, "{}");
    mocks.deleteServerFileUpload
      .mockRejectedValueOnce({ status: 409, code: "upload_busy", message: "" })
      .mockResolvedValueOnce(null);

    await discardSession("lobby", "world.zip", "s1", sleep);

    expect(localStorage.getItem(KEY)).toBeNull();
    expect(mocks.deleteServerFileUpload.mock.calls).toEqual([
      ["lobby", "s1"],
      ["lobby", "s1"],
    ]);
  });

  it("tries three times at most, and never past another refusal", async () => {
    mocks.deleteServerFileUpload.mockRejectedValue({ status: 409, code: "upload_busy", message: "" });
    await discardSession("lobby", "world.zip", "s1", sleep);
    expect(mocks.deleteServerFileUpload).toHaveBeenCalledTimes(3);

    mocks.deleteServerFileUpload.mockReset();
    mocks.deleteServerFileUpload.mockRejectedValue(gone);
    await discardSession("lobby", "world.zip", "s1", sleep);
    expect(mocks.deleteServerFileUpload).toHaveBeenCalledTimes(1);
  });

  it("forgetSession drops only the one path", () => {
    localStorage.setItem(KEY, "{}");
    localStorage.setItem("felis-file-upload:lobby:other.zip", "{}");

    forgetSession("lobby", "world.zip");

    expect(localStorage.getItem(KEY)).toBeNull();
    expect(localStorage.getItem("felis-file-upload:lobby:other.zip")).toBe("{}");
  });
});
