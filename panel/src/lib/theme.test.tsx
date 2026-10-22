// @vitest-environment jsdom
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ThemeProvider, useTheme } from "./theme";

let systemDark = false;
const listeners = new Set<() => void>();

beforeEach(() => {
  window.localStorage.clear();
  document.documentElement.className = "";
  systemDark = false;
  listeners.clear();
  vi.stubGlobal("matchMedia", () => ({
    get matches() { return systemDark; },
    addEventListener: (_event: string, listener: () => void) => listeners.add(listener),
    removeEventListener: (_event: string, listener: () => void) => listeners.delete(listener),
  }));
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.localStorage.clear();
  document.documentElement.className = "";
});

function ThemeChoices() {
  const { setTheme } = useTheme();
  return <>{(["light", "dark", "system"] as const).map((choice) => <button key={choice} onClick={() => setTheme(choice)}>{choice}</button>)}</>;
}

function showChoices() {
  return render(<ThemeProvider><ThemeChoices /></ThemeProvider>);
}

function systemChanges(dark: boolean) {
  act(() => {
    systemDark = dark;
    listeners.forEach((listener) => listener());
  });
}

describe("theme preference", () => {
  it.each([null, "system", "obsolete"])("follows system changes for preference %s", (saved) => {
    if (saved !== null) window.localStorage.setItem("felis-theme", saved);
    showChoices();
    expect(document.documentElement.classList.contains("light")).toBe(true);
    systemChanges(true);
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    systemChanges(false);
    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });

  it.each(["light", "dark"] as const)("remembers %s after reopening and ignores system changes", async (choice) => {
    const view = showChoices();
    await userEvent.click(screen.getByRole("button", { name: choice }));
    systemChanges(choice === "light");
    expect(document.documentElement.classList.contains(choice)).toBe(true);
    view.unmount();
    showChoices();
    expect(document.documentElement.classList.contains(choice)).toBe(true);
  });

  it("can resume following the system and remembers that choice", async () => {
    window.localStorage.setItem("felis-theme", "dark");
    const view = showChoices();
    await userEvent.click(screen.getByRole("button", { name: "system" }));
    expect(document.documentElement.classList.contains("light")).toBe(true);
    view.unmount();
    showChoices();
    systemChanges(true);
    expect(document.documentElement.classList.contains("dark")).toBe(true);
  });

  it("keeps theme selection usable when browser storage is denied", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new DOMException("Storage denied", "SecurityError"); });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new DOMException("Storage denied", "SecurityError"); });
    showChoices();
    systemChanges(true);
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    await userEvent.click(screen.getByRole("button", { name: "light" }));
    expect(document.documentElement.classList.contains("light")).toBe(true);
  });
});

describe("theme before React starts", () => {
  const html = readFileSync("index.html", "utf8");
  const script = html.match(/<script>([\s\S]*?)<\/script>/)![1];

  it.each([
    { saved: "dark", system: false, dark: true },
    { saved: "light", system: true, dark: false },
    { saved: "system", system: true, dark: true },
    { saved: "system", system: false, dark: false },
    { saved: "obsolete", system: true, dark: true },
  ])("applies $saved before paint with system dark $system", ({ saved, system, dark }) => {
    window.localStorage.setItem("felis-theme", saved);
    systemDark = system;
    new Function(script)();
    expect(document.documentElement.classList.contains("dark")).toBe(dark);
  });
});
