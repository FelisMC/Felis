// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { GiBInput, gibQuantity, hasExplicitUnit, isWholeGiB, parseGiB, quantityGiB } from "./GiBInput";

describe("parseGiB and quantity formatting", () => {
  it.each([
    ["4", 4, "4Gi"],
    ["4GiB", 4, "4Gi"],
    ["4 GB", 4, "4Gi"],
    ["4g", 4, "4Gi"],
    ["1.5Gi", 1.5, "1536Mi"],
    ["1536MiB", 1.5, "1536Mi"],
    ["1536 MB", 1.5, "1536Mi"],
    ["2048m", 2, "2Gi"],
    ["10240MiB", 10, "10Gi"],
  ])("parses %j into %s GiB (%s)", (input, gib, quantity) => {
    const parsed = parseGiB(input);
    expect(parsed).toBeCloseTo(gib, 6);
    expect(gibQuantity(parsed!)).toBe(quantity);
  });

  it("identifies whole GiB values including MiB equivalents", () => {
    expect(isWholeGiB(parseGiB("10GB")!)).toBe(true);
    expect(isWholeGiB(parseGiB("10240MiB")!)).toBe(true);
    expect(isWholeGiB(parseGiB("1536MiB")!)).toBe(false);
    expect(isWholeGiB(parseGiB("7.5")!)).toBe(false);
  });

  it.each(["", "abc", "-2", "4TB", "4KiB", "2.5.1GB", "GB"])("rejects invalid size %j", (bad) => {
    expect(parseGiB(bad)).toBeNull();
  });

  it("normalizes server quantities to GiB strings", () => {
    expect(quantityGiB("4Gi")).toBe("4");
    expect(quantityGiB("1536Mi")).toBe("1.5");
    expect(quantityGiB("bad")).toBe("");
  });
});

describe("GiBInput suffix display", () => {
  it("shows GiB suffix for bare numbers and hides it once an explicit unit is typed", () => {
    const { rerender } = render(<GiBInput id="mem" value="4" onChange={vi.fn()} />);
    expect(hasExplicitUnit("4")).toBe(false);
    expect(screen.getByText("GiB")).toBeTruthy();

    rerender(<GiBInput id="mem" value="4GB" onChange={vi.fn()} />);
    expect(hasExplicitUnit("4GB")).toBe(true);
    expect(screen.queryByText("GiB")).toBeNull();

    rerender(<GiBInput id="mem" value="2048MB" onChange={vi.fn()} />);
    expect(screen.queryByText("GiB")).toBeNull();
  });
});
