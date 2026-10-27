// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import i18next from "i18next";
import { ToastProvider, useToast } from "./Toaster";

function Trigger({ label, kind, message }: { label: string; kind: "success" | "error"; message: string }) {
  const toast = useToast();
  return <button onClick={() => toast[kind](message)}>{label}</button>;
}

const notifications = () => screen.getByRole("region", { name: i18next.t("common:notifications") });

afterEach(() => {
  vi.useRealTimers();
});

describe("Toaster", () => {
  it("shows a success notice in the polite region and removes it after five seconds", () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Trigger label="save" kind="success" message="Saved notes" />
      </ToastProvider>,
    );
    expect(within(notifications()).getByRole("status").textContent).toBe("");
    expect(within(notifications()).getByRole("alert").textContent).toBe("");

    fireEvent.click(screen.getByRole("button", { name: "save" }));
    expect(within(screen.getByRole("status")).getByText("Saved notes")).toBeTruthy();

    act(() => vi.advanceTimersByTime(4999));
    expect(screen.getByText("Saved notes")).toBeTruthy();
    act(() => vi.advanceTimersByTime(1));
    expect(screen.queryByText("Saved notes")).toBeNull();
  });

  it("keeps an error notice in the assertive region until it is dismissed", () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Trigger label="upload" kind="error" message="Upload failed" />
      </ToastProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "upload" }));
    expect(within(screen.getByRole("alert")).getByText("Upload failed")).toBeTruthy();

    act(() => vi.advanceTimersByTime(10_000));
    expect(screen.getByText("Upload failed")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: i18next.t("common:dismiss_notification") }));
    expect(screen.queryByText("Upload failed")).toBeNull();
  });

  it("keeps only the newest three notices when a fourth arrives", () => {
    render(
      <ToastProvider>
        <Trigger label="first" kind="success" message="Notice 1" />
        <Trigger label="second" kind="success" message="Notice 2" />
        <Trigger label="third" kind="error" message="Notice 3" />
        <Trigger label="fourth" kind="success" message="Notice 4" />
      </ToastProvider>,
    );
    for (const label of ["first", "second", "third", "fourth"]) {
      fireEvent.click(screen.getByRole("button", { name: label }));
    }
    expect(screen.queryByText("Notice 1")).toBeNull();
    for (const message of ["Notice 2", "Notice 3", "Notice 4"]) {
      expect(screen.getByText(message)).toBeTruthy();
    }
  });

  it("accepts notices outside a provider without showing anything", () => {
    render(<Trigger label="save" kind="error" message="Failed" />);
    expect(() => fireEvent.click(screen.getByRole("button", { name: "save" }))).not.toThrow();
    expect(screen.queryByRole("region", { name: i18next.t("common:notifications") })).toBeNull();
  });
});
