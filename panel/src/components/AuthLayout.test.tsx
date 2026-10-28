// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import i18next from "i18next";
import { AuthLayout } from "./AuthLayout";
import { DEFAULT_BRANDING } from "@/lib/branding";
import type { PublicBranding } from "@/lib/types";

const branding = vi.hoisted(() => ({ value: null as PublicBranding | null }));
vi.mock("@/lib/branding", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/branding")>();
  return { ...actual, useBranding: () => branding.value ?? actual.DEFAULT_BRANDING };
});
vi.mock("@/lib/hooks", () => ({ useConfig: () => null }));

afterEach(() => {
  branding.value = null;
});

function renderLayout() {
  return render(<AuthLayout title="Sign in" subtitle="Player console"><p>form</p></AuthLayout>);
}

describe("AuthLayout branding", () => {
  it("shows the notice as plain text, so markup in it stays visible as typed", () => {
    branding.value = { ...DEFAULT_BRANDING, notice: "<b>Scheduled</b> maintenance\nBack at 9" };
    const { container } = renderLayout();
    expect(screen.getByText(/<b>Scheduled<\/b> maintenance/)).toBeTruthy();
    expect(container.querySelector("b")).toBeNull();
    expect(container.textContent).toContain("maintenance\nBack at 9");
  });

  it("names the browser tab after the configured name and shows the logo and background", () => {
    branding.value = { ...DEFAULT_BRANDING, name: "Acme Cloud", logo: "/api/v1/branding/logo?v=a", background: "/api/v1/branding/background?v=b" };
    const { container } = renderLayout();
    expect(document.title).toBe("Sign in · Acme Cloud");
    expect(screen.getByRole("img", { name: "Acme Cloud" }).getAttribute("src")).toBe("/api/v1/branding/logo?v=a");
    expect(container.querySelector('img[src="/api/v1/branding/background?v=b"]')).toBeTruthy();
    expect(screen.getByText("form")).toBeTruthy();
  });

  it("credits Felis at the bottom unless the Owner turned the credit off", () => {
    const credit = () => screen.queryByRole("link", { name: i18next.t("common:powered_by") });
    const { unmount } = renderLayout();
    expect(credit()?.getAttribute("href")).toBe("https://felismc.com");
    unmount();
    branding.value = { ...DEFAULT_BRANDING, show_powered_by: false };
    renderLayout();
    expect(credit()).toBeNull();
  });
});
