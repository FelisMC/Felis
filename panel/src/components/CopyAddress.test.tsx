// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { CopyAddress } from "./CopyAddress";

function stubClipboard(writeText: (text: string) => Promise<void>) {
  const fn = vi.fn(writeText);
  Object.defineProperty(navigator, "clipboard", { value: { writeText: fn }, configurable: true });
  return fn;
}

afterEach(() => {
  vi.useRealTimers();
  Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
  window.getSelection()?.removeAllRanges();
});

describe("CopyAddress", () => {
  it("shows the address as text, never as a web link", () => {
    render(<CopyAddress address="survival.mc.example:25570" />);
    expect(screen.getByText("survival.mc.example:25570")).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("copies the address and says so, then settles back", async () => {
    vi.useFakeTimers();
    const writeText = stubClipboard(async () => {});
    render(<CopyAddress address="survival.mc.example:25570" />);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy address survival.mc.example:25570" }));
    });
    expect(writeText).toHaveBeenCalledWith("survival.mc.example:25570");
    expect(screen.getByRole("status").textContent).toBe("Copied");

    act(() => {
      vi.advanceTimersByTime(2000);
    });
    expect(screen.getByRole("status").textContent).toBe("");
  });

  it("selects the address for the system menu when the clipboard is refused", async () => {
    stubClipboard(async () => {
      throw new DOMException("denied", "NotAllowedError");
    });
    render(<CopyAddress address="survival.mc.example" />);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy address survival.mc.example" }));
    });
    expect(window.getSelection()?.toString()).toBe("survival.mc.example");
    expect(screen.getByRole("status").textContent).toBe("Selected — copy it from the menu");
  });

  it("selects the address where there is no clipboard at all", async () => {
    render(<CopyAddress address="survival.mc.example" />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy address survival.mc.example" }));
    });
    expect(window.getSelection()?.toString()).toBe("survival.mc.example");
  });
});
