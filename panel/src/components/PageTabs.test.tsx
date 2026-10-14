// @vitest-environment jsdom
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { PageTabs } from "./PageTabs";
import { MainScrollContext } from "./MainScroll";

const sections = [
  { key: "status", label: "Status" },
  { key: "backups", label: "Backups" },
  { key: "updates", label: "Updates" },
] as const;
type Section = (typeof sections)[number]["key"];

function Page({ onChange }: { onChange?: (key: Section) => void }) {
  const [value, setValue] = useState<Section>("status");
  return <PageTabs label="Sections" idPrefix="demo" value={value} tabs={sections} onChange={(key) => { onChange?.(key); setValue(key); }} />;
}

describe("page tabs", () => {
  it("keeps only the chosen tab in the tab order and points it at its panel", () => {
    render(<Page />);
    const [status, backups] = screen.getAllByRole("tab");
    expect(screen.getByRole("tablist", { name: "Sections" })).toBeTruthy();
    expect(status.getAttribute("aria-selected")).toBe("true");
    expect(status.tabIndex).toBe(0);
    expect(status.getAttribute("aria-controls")).toBe("demo-panel-status");
    expect(backups.getAttribute("aria-selected")).toBe("false");
    expect(backups.tabIndex).toBe(-1);
    expect(backups.getAttribute("aria-controls")).toBeNull();
  });

  it("moves with the arrow keys, Home and End, choosing and focusing as it goes", () => {
    const onChange = vi.fn();
    render(<Page onChange={onChange} />);
    const list = screen.getByRole("tablist");
    const tab = (name: string) => screen.getByRole("tab", { name });
    fireEvent.keyDown(list, { key: "ArrowRight" });
    expect(tab("Backups").getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(tab("Backups"));
    fireEvent.keyDown(list, { key: "End" });
    expect(document.activeElement).toBe(tab("Updates"));
    fireEvent.keyDown(list, { key: "ArrowRight" });
    expect(tab("Status").getAttribute("aria-selected")).toBe("true");
    fireEvent.keyDown(list, { key: "ArrowLeft" });
    expect(tab("Updates").getAttribute("aria-selected")).toBe("true");
    fireEvent.keyDown(list, { key: "Home" });
    expect(document.activeElement).toBe(tab("Status"));
    fireEvent.keyDown(list, { key: "Enter" });
    expect(onChange.mock.calls.map(([key]) => key)).toEqual(["backups", "updates", "status", "updates", "status"]);
  });

  it("brings the content area back to its top once another section shows, and not before", async () => {
    const main = { scrollTop: 640 } as unknown as HTMLElement;
    function Held() {
      const [value, setValue] = useState<Section>("status");
      const [held, setHeld] = useState(true);
      return <MainScrollContext.Provider value={main}>
        <button type="button" onClick={() => setHeld(false)}>Discard</button>
        <PageTabs label="Sections" idPrefix="demo" value={value} tabs={sections} onChange={(key) => { if (!held) setValue(key); }} />
      </MainScrollContext.Provider>;
    }
    render(<Held />);
    expect(main.scrollTop).toBe(640);
    await userEvent.click(screen.getByRole("tab", { name: "Backups" }));
    expect(main.scrollTop).toBe(640);
    await userEvent.click(screen.getByRole("button", { name: "Discard" }));
    await userEvent.click(screen.getByRole("tab", { name: "Backups" }));
    expect(screen.getByRole("tab", { name: "Backups" }).getAttribute("aria-selected")).toBe("true");
    expect(main.scrollTop).toBe(0);
  });

  it("chooses a tab on click and leaves a click on the chosen one alone", async () => {
    const onChange = vi.fn();
    render(<Page onChange={onChange} />);
    await userEvent.click(screen.getByRole("tab", { name: "Status" }));
    expect(onChange).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("tab", { name: "Updates" }));
    expect(onChange).toHaveBeenCalledWith("updates");
  });
});
