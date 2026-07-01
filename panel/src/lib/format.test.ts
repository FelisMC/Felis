import { describe, it, expect } from "vitest";
import { formatBytes, formatRelative, formatAbsolute, isExpired } from "./format";

describe("formatBytes", () => {
  it("renders sub-KiB counts as plain bytes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
  });

  it("steps up binary units, one decimal below 10 and none above", () => {
    expect(formatBytes(1024)).toBe("1.0 KiB");
    expect(formatBytes(1024 * 1024)).toBe("1.0 MiB");
    expect(formatBytes(1.4 * 1024 * 1024 * 1024)).toBe("1.4 GiB");
    expect(formatBytes(140 * 1024 * 1024)).toBe("140 MiB");
  });

  it("caps at PiB and never overflows the unit list", () => {
    expect(formatBytes(5 * 1024 ** 5)).toBe("5.0 PiB");
    expect(formatBytes(5000 * 1024 ** 5)).toBe("5000 PiB");
  });

  it("renders a non-finite or negative input as an em dash, never NaN", () => {
    expect(formatBytes(-1)).toBe("—");
    expect(formatBytes(NaN)).toBe("—");
    expect(formatBytes(Infinity)).toBe("—");
  });
});

describe("formatRelative (now injected for determinism)", () => {
  const now = Date.parse("2026-07-01T12:00:00Z");

  it("renders past timestamps", () => {
    expect(formatRelative("2026-07-01T09:00:00Z", now, "en-US")).toBe("3 hours ago");
    expect(formatRelative("2026-06-28T12:00:00Z", now, "en-US")).toBe("3 days ago");
  });

  it("renders future timestamps (retention deadlines)", () => {
    expect(formatRelative("2026-07-26T12:00:00Z", now, "en-US")).toBe("in 25 days");
  });

  it("rolls exactly-30-days up into the month bucket (boundary)", () => {
    // 30 days is the day-bucket's exclusive upper edge, so it reads as a month.
    expect(formatRelative("2026-07-31T12:00:00Z", now, "en-US")).toBe("next month");
  });

  it("localizes into zh-CN", () => {
    // Intl carries the localization; assert it is non-empty and not the English form.
    const zh = formatRelative("2026-06-28T12:00:00Z", now, "zh-CN");
    expect(zh).not.toBe("");
    expect(zh).not.toContain("ago");
  });

  it("returns empty string for an unparseable input", () => {
    expect(formatRelative("not-a-date", now, "en-US")).toBe("");
  });
});

describe("formatAbsolute", () => {
  it("returns empty string for an unparseable input", () => {
    expect(formatAbsolute("nope", "en-US")).toBe("");
  });
  it("renders a non-empty localized string for a valid input", () => {
    expect(formatAbsolute("2026-07-01T12:00:00Z", "en-US")).not.toBe("");
  });
});

describe("isExpired", () => {
  const now = Date.parse("2026-07-01T12:00:00Z");
  it("is true at or before now, false after", () => {
    expect(isExpired("2026-07-01T11:59:59Z", now)).toBe(true);
    expect(isExpired("2026-07-01T12:00:00Z", now)).toBe(true);
    expect(isExpired("2026-07-01T12:00:01Z", now)).toBe(false);
  });
  it("is false for an unparseable input (never blocks on garbage)", () => {
    expect(isExpired("nope", now)).toBe(false);
  });
});
