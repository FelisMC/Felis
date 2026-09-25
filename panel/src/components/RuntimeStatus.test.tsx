// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import i18next from "i18next";
import type { RuntimeConfig } from "@/lib/config";
import { ConfigBanner, VersionBadge } from "./RuntimeStatus";

const state = vi.hoisted(() => ({ cfg: null as RuntimeConfig | null }));
vi.mock("@/lib/hooks", () => ({ useConfig: () => state.cfg }));

const base: RuntimeConfig = { apiBase: "/api/v1", rootDomain: "mc.example" };

beforeEach(() => {
  state.cfg = null;
});

describe("VersionBadge", () => {
  it("shows the release of a clean build, with the full stamp as its tooltip", () => {
    state.cfg = { ...base, build: { version: "v1.2.0", release: "v1.2.0", dev: false } };
    const { container } = render(<VersionBadge />);
    expect(container.textContent).toBe("v1.2.0");
    expect(container.firstElementChild?.getAttribute("title")).toBe("v1.2.0");
  });

  it("marks a dev build and shows its commit", () => {
    state.cfg = { ...base, build: { version: "v1.2.0+g1a2b3c4", release: "v1.2.0", commit: "1a2b3c4", dev: true } };
    render(<VersionBadge />);
    expect(screen.getByText("v1.2.0+1a2b3c4")).toBeTruthy();
    expect(screen.getByText(i18next.t("common:build_dev"))).toBeTruthy();
  });

  it("shows only the name without a build stamp", () => {
    state.cfg = base;
    const { container } = render(<VersionBadge />);
    expect(container.textContent).toBe("Felis");
  });
});

describe("ConfigBanner", () => {
  it("stays hidden while the config loads and once it loaded", () => {
    const { container, rerender } = render(<ConfigBanner />);
    expect(container.textContent).toBe("");
    state.cfg = base;
    rerender(<ConfigBanner />);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("warns when the panel runs on fallback defaults, and reloads on request", () => {
    state.cfg = { ...base, fallback: true };
    const reload = vi.fn();
    Object.defineProperty(window, "location", { value: { ...window.location, reload }, configurable: true });
    render(<ConfigBanner />);

    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain(i18next.t("common:config_unavailable"));
    fireEvent.click(screen.getByRole("button", { name: i18next.t("common:reload_page") }));
    expect(reload).toHaveBeenCalled();
  });
});
