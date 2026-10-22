import { describe, expect, it } from "vitest";
import { cpuUsage, memoryLimit, memoryUsage } from "./resourceUsage";

describe("resource usage", () => {
  it.each([
    [500, "2", "25.0%"],
    [250, "500m", "50.0%"],
    [0, "1", "0.0%"],
    [250, undefined, "250 mCPU"],
    [undefined, "2", "—"],
  ])("shows %s millicores against %s as %s", (milli, limit, expected) => {
    expect(cpuUsage(milli, limit)).toBe(expected);
  });
  it.each([
    ["1536Mi", 1536 * 1024 ** 2], ["2Gi", 2 * 1024 ** 3], ["2G", 2e9], ["2e9", 2e9], ["4096", 4096], ["invalid", null], [undefined, null],
  ])("reads memory limit %s", (limit, expected) => expect(memoryLimit(limit)).toBe(expected));
  it("keeps missing usage separate from an idle server", () => {
    expect(memoryUsage(undefined, "2Gi")).toBe("—");
    expect(memoryUsage(0, "2Gi")).toBe("0 B / 2.0 GiB");
    expect(memoryUsage(512 * 1024 ** 2, "2Gi")).toBe("512 MiB / 2.0 GiB");
  });
});
