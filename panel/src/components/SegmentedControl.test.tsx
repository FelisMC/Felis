// @vitest-environment jsdom
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { SegmentedControl } from "./SegmentedControl";

type Filter = "all" | "pending" | "done";

function Filters({ onChange }: { onChange?: (key: Filter) => void }) {
  const [value, setValue] = useState<Filter>("all");
  return <SegmentedControl label="Status" value={value} onChange={(key) => { onChange?.(key); setValue(key); }}
    segments={[{ key: "all", label: "All", count: 9 }, { key: "pending", label: "Pending", count: 2 }, { key: "done", label: "Done" }]} />;
}

describe("segmented control", () => {
  it("is a radio group whose segments are read with their counts", () => {
    render(<Filters />);
    expect(screen.getByRole("radiogroup", { name: "Status" })).toBeTruthy();
    const all = screen.getByRole("radio", { name: "All (9)" });
    expect(all.getAttribute("aria-checked")).toBe("true");
    expect(all.tabIndex).toBe(0);
    const done = screen.getByRole("radio", { name: "Done" });
    expect(done.getAttribute("aria-checked")).toBe("false");
    expect(done.tabIndex).toBe(-1);
  });

  it("moves with the arrow keys, Home and End, and wraps around", () => {
    const onChange = vi.fn();
    render(<Filters onChange={onChange} />);
    const group = screen.getByRole("radiogroup");
    fireEvent.keyDown(group, { key: "ArrowLeft" });
    expect(document.activeElement).toBe(screen.getByRole("radio", { name: "Done" }));
    fireEvent.keyDown(group, { key: "ArrowDown" });
    fireEvent.keyDown(group, { key: "End" });
    fireEvent.keyDown(group, { key: "Home" });
    fireEvent.keyDown(group, { key: "ArrowRight" });
    fireEvent.keyDown(group, { key: "a" });
    expect(onChange.mock.calls.map(([key]) => key)).toEqual(["done", "all", "done", "all", "pending"]);
    expect(screen.getByRole("radio", { name: "Pending (2)" }).getAttribute("aria-checked")).toBe("true");
  });

  it("chooses on click and leaves a click on the chosen segment alone", async () => {
    const onChange = vi.fn();
    render(<Filters onChange={onChange} />);
    await userEvent.click(screen.getByRole("radio", { name: "All (9)" }));
    expect(onChange).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("radio", { name: "Pending (2)" }));
    expect(onChange).toHaveBeenCalledWith("pending");
  });
});
