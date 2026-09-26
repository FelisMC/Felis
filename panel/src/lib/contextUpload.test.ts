import { describe, it, expect, vi, beforeEach } from "vitest";

const calls = vi.hoisted(() => ({
  getContextUpload: vi.fn(),
  putContextPart: vi.fn(),
  completeContextUpload: vi.fn(),
}));
vi.mock("./api", () => ({ api: calls }));

const { uploadContext } = await import("./contextUpload");

// A 10-byte "file" and a server that takes parts of at most 4 bytes.
const FILE = new Blob(["0123456789"]);
const at = (received: number) => ({ received, part_max_bytes: 4, max_context_bytes: 1073741824 });
const dropped = { status: 0, code: "network_error", message: "Failed to fetch" };
const edgeTimeout = { status: 524, code: "upstream_unavailable", message: "" };

// A server that appends each part where it says and answers the new length.
function acceptParts() {
  calls.putContextPart.mockImplementation(async (_id: string, offset: number, part: Blob) => at(offset + part.size));
}

async function sentParts(): Promise<Array<[number, string]>> {
  return Promise.all(
    calls.putContextPart.mock.calls.map(async ([, offset, part]) => [offset as number, await (part as Blob).text()] as [number, string]),
  );
}

const sleep = vi.fn(async (_ms: number, _signal?: AbortSignal) => undefined);

beforeEach(() => {
  calls.getContextUpload.mockReset();
  calls.putContextPart.mockReset();
  calls.completeContextUpload.mockReset();
  calls.completeContextUpload.mockResolvedValue({ id: "sub-1" });
  sleep.mockClear();
});

describe("uploadContext", () => {
  it("sends the file in parts no larger than the server's cap, then stores it", async () => {
    calls.getContextUpload.mockResolvedValue(at(0));
    acceptParts();
    const progress: Array<[number, number]> = [];
    await uploadContext("sub-1", FILE, { sleep, onProgress: (sent, total) => progress.push([sent, total]) });
    expect(await sentParts()).toEqual([
      [0, "0123"],
      [4, "4567"],
      [8, "89"],
    ]);
    expect(calls.putContextPart.mock.calls.map((c) => c[0])).toEqual(["sub-1", "sub-1", "sub-1"]);
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(1);
    expect(calls.completeContextUpload).toHaveBeenCalledWith("sub-1");
    expect(progress.at(-1)).toEqual([10, 10]);
  });

  it("reports the bytes of a part in flight on top of the parts already stored", async () => {
    calls.getContextUpload.mockResolvedValue(at(0));
    calls.putContextPart.mockImplementation(async (_id, offset: number, part: Blob, opts) => {
      opts.onProgress(2);
      return at(offset + part.size);
    });
    const progress: Array<[number, number, number]> = [];
    await uploadContext("sub-1", FILE, {
      sleep,
      onProgress: (sent, total, stored) => progress.push([sent, total, stored]),
    });
    expect(progress).toEqual([
      [0, 10, 0],
      [2, 10, 0],
      [4, 10, 4],
      [6, 10, 4],
      [8, 10, 8],
      [10, 10, 8],
      [10, 10, 10],
    ]);
  });

  it("starts a fresh file over even when the submission has parts staged", async () => {
    calls.getContextUpload.mockResolvedValue(at(4));
    acceptParts();
    await uploadContext("sub-1", FILE, { sleep });
    expect((await sentParts()).map(([o]) => o)).toEqual([0, 4, 8]);
  });

  it("resumes the same file from where the staged bytes end", async () => {
    calls.getContextUpload.mockResolvedValue(at(4));
    acceptParts();
    await uploadContext("sub-1", FILE, { sleep, resume: true });
    expect(await sentParts()).toEqual([
      [4, "4567"],
      [8, "89"],
    ]);
  });

  it("goes straight to storing when every byte is already staged", async () => {
    calls.getContextUpload.mockResolvedValue(at(10));
    await uploadContext("sub-1", FILE, { sleep, resume: true });
    expect(calls.putContextPart).not.toHaveBeenCalled();
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(1);
  });

  it("starts over when more is staged than the file holds", async () => {
    calls.getContextUpload.mockResolvedValue(at(12));
    acceptParts();
    await uploadContext("sub-1", FILE, { sleep, resume: true });
    expect((await sentParts()).map(([o]) => o)).toEqual([0, 4, 8]);
  });

  it("after a dropped part asks where the upload stands and carries on from there", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockResolvedValueOnce(at(4));
    acceptParts();
    calls.putContextPart.mockImplementationOnce(async (_id, offset: number, part: Blob) => at(offset + part.size));
    calls.putContextPart.mockRejectedValueOnce(dropped);
    await uploadContext("sub-1", FILE, { sleep });
    expect((await sentParts()).map(([o]) => o)).toEqual([0, 4, 4, 8]);
    expect(calls.getContextUpload).toHaveBeenCalledTimes(2);
    expect(sleep.mock.calls.map((c) => c[0])).toEqual([1000]);
  });

  it("a fresh file whose first part dropped starts over at 0", async () => {
    calls.getContextUpload.mockResolvedValue(at(2));
    acceptParts();
    calls.putContextPart.mockRejectedValueOnce(dropped);
    await uploadContext("sub-1", FILE, { sleep });
    expect((await sentParts()).map(([o]) => o)).toEqual([0, 0, 4, 8]);
  });

  it("follows the server when it says the upload holds a different length", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockResolvedValueOnce(at(8));
    acceptParts();
    calls.putContextPart.mockImplementationOnce(async () => at(4));
    calls.putContextPart.mockRejectedValueOnce({ status: 409, code: "upload_offset_mismatch", message: "" });
    await uploadContext("sub-1", FILE, { sleep });
    expect((await sentParts()).map(([o]) => o)).toEqual([0, 4, 8]);
  });

  it("sends a part again when the uploads store did not answer the budget check", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockResolvedValueOnce(at(4));
    acceptParts();
    calls.putContextPart.mockImplementationOnce(async (_id, offset: number, part: Blob) => at(offset + part.size));
    calls.putContextPart.mockRejectedValueOnce({ status: 503, code: "uploads_store_unavailable", message: "" });
    await uploadContext("sub-1", FILE, { sleep });
    expect((await sentParts()).map(([o]) => o)).toEqual([0, 4, 4, 8]);
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(1);
  });

  it("gives up at once on a refusal the next attempt cannot outlast", async () => {
    calls.getContextUpload.mockResolvedValue(at(0));
    const refused = { status: 400, code: "bad_request", message: "context must be a gzip-compressed tarball" };
    calls.putContextPart.mockRejectedValue(refused);
    await expect(uploadContext("sub-1", FILE, { sleep })).rejects.toBe(refused);
    expect(calls.putContextPart).toHaveBeenCalledTimes(1);
    expect(sleep).not.toHaveBeenCalled();
    expect(calls.completeContextUpload).not.toHaveBeenCalled();
  });

  it("gives up after eight dropped attempts in a row, backing off up to 15 s", async () => {
    calls.getContextUpload.mockResolvedValue(at(0));
    calls.putContextPart.mockRejectedValue(dropped);
    await expect(uploadContext("sub-1", FILE, { sleep })).rejects.toBe(dropped);
    expect(calls.putContextPart).toHaveBeenCalledTimes(8);
    expect(sleep.mock.calls.map((c) => c[0])).toEqual([1000, 2000, 4000, 8000, 15000, 15000, 15000]);
  });

  it("a part that gets through resets the count of dropped attempts", async () => {
    calls.getContextUpload.mockImplementation(async () => at(calls.putContextPart.mock.calls.length >= 8 ? 4 : 0));
    acceptParts();
    for (let i = 0; i < 7; i++) calls.putContextPart.mockRejectedValueOnce(dropped);
    calls.putContextPart.mockImplementationOnce(async () => at(4));
    for (let i = 0; i < 7; i++) calls.putContextPart.mockRejectedValueOnce(dropped);
    await uploadContext("sub-1", FILE, { sleep });
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(1);
  });

  it("sends an empty file as one part, so the server gives the refusal", async () => {
    calls.getContextUpload.mockResolvedValue(at(0));
    const refused = { status: 400, code: "bad_request", message: "context must be a gzip-compressed tarball" };
    calls.putContextPart.mockRejectedValue(refused);
    await expect(uploadContext("sub-1", new Blob([]), { sleep })).rejects.toBe(refused);
    expect(calls.putContextPart.mock.calls.map((c) => [c[1], (c[2] as Blob).size])).toEqual([[0, 0]]);
  });

  it("stops on a pause without retrying", async () => {
    calls.getContextUpload.mockResolvedValue(at(0));
    const paused = new DOMException("The upload was cancelled", "AbortError");
    calls.putContextPart.mockRejectedValue(paused);
    await expect(uploadContext("sub-1", FILE, { sleep })).rejects.toBe(paused);
    expect(calls.putContextPart).toHaveBeenCalledTimes(1);
    expect(sleep).not.toHaveBeenCalled();
  });

  it("passes the pause signal to every part", async () => {
    calls.getContextUpload.mockResolvedValue(at(0));
    acceptParts();
    const controller = new AbortController();
    await uploadContext("sub-1", FILE, { sleep, signal: controller.signal });
    expect(calls.putContextPart.mock.calls.map((c) => c[3].signal)).toEqual([
      controller.signal,
      controller.signal,
      controller.signal,
    ]);
  });
});

describe("uploadContext completion", () => {
  beforeEach(() => {
    acceptParts();
  });

  it("treats the staged upload vanishing after an edge timeout as stored", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockResolvedValueOnce(at(0));
    calls.completeContextUpload.mockRejectedValueOnce(edgeTimeout);
    await uploadContext("sub-1", FILE, { sleep });
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(1);
    expect(calls.getContextUpload).toHaveBeenCalledTimes(2);
  });

  it("waits out a store still in progress, then takes its answer", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockResolvedValue(at(10));
    calls.completeContextUpload
      .mockRejectedValueOnce(edgeTimeout)
      .mockRejectedValueOnce({ status: 409, code: "upload_busy", message: "" })
      .mockResolvedValueOnce({ id: "sub-1" });
    await uploadContext("sub-1", FILE, { sleep });
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(3);
    expect(sleep.mock.calls.map((c) => c[0])).toEqual([1000, 2000]);
  });

  it("completes again when the uploads store did not answer, the staged bytes still all there", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockResolvedValue(at(10));
    calls.completeContextUpload
      .mockRejectedValueOnce({ status: 503, code: "uploads_store_unavailable", message: "" })
      .mockResolvedValueOnce({ id: "sub-1" });
    await uploadContext("sub-1", FILE, { sleep });
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(2);
  });

  it("surfaces the real refusal when completing again answers with one", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockResolvedValue(at(10));
    const quota = { status: 403, code: "submission_quota_exceeded", message: "budget" };
    calls.completeContextUpload.mockRejectedValueOnce(edgeTimeout).mockRejectedValueOnce(quota);
    await expect(uploadContext("sub-1", FILE, { sleep })).rejects.toBe(quota);
  });

  it("surfaces a refusal to store at once", async () => {
    calls.getContextUpload.mockResolvedValue(at(0));
    const refused = { status: 429, code: "submission_cooldown", message: "" };
    calls.completeContextUpload.mockRejectedValueOnce(refused);
    await expect(uploadContext("sub-1", FILE, { sleep })).rejects.toBe(refused);
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(1);
    expect(sleep).not.toHaveBeenCalled();
  });

  it("reports the timeout when the staged upload is cut short while storing", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockResolvedValueOnce(at(6));
    calls.completeContextUpload.mockRejectedValueOnce(edgeTimeout);
    await expect(uploadContext("sub-1", FILE, { sleep })).rejects.toBe(edgeTimeout);
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(1);
  });

  it("ends the wait when checking on the store is refused", async () => {
    const gone = { status: 404, code: "not_found", message: "submission not found" };
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockRejectedValueOnce(gone);
    calls.completeContextUpload.mockRejectedValueOnce(edgeTimeout);
    await expect(uploadContext("sub-1", FILE, { sleep })).rejects.toBe(gone);
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(1);
  });

  it("keeps waiting through a check that did not get through", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockRejectedValueOnce(dropped).mockResolvedValueOnce(at(0));
    calls.completeContextUpload.mockRejectedValueOnce(edgeTimeout);
    await uploadContext("sub-1", FILE, { sleep });
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(1);
    expect(sleep.mock.calls.map((c) => c[0])).toEqual([1000, 2000]);
  });

  it("gives up after forty checks on a store that never finishes", async () => {
    calls.getContextUpload.mockResolvedValueOnce(at(0)).mockResolvedValue(at(10));
    calls.completeContextUpload.mockRejectedValue(edgeTimeout);
    await expect(uploadContext("sub-1", FILE, { sleep })).rejects.toBe(edgeTimeout);
    expect(calls.completeContextUpload).toHaveBeenCalledTimes(41);
  });
});
