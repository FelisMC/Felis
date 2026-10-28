// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BrandingProvider, DEFAULT_BRANDING, useBranding, useRefreshBranding } from "./branding";
import type { PublicBranding } from "./types";

const calls = vi.hoisted(() => ({ getBranding: vi.fn() }));
vi.mock("@/lib/api", () => ({ api: calls }));

const acme: PublicBranding = { ...DEFAULT_BRANDING, name: "Acme Cloud" };

function Probe() {
  const { name } = useBranding();
  const refresh = useRefreshBranding();
  return <><p>name: {name}</p><button onClick={refresh}>reread</button></>;
}

function mount() {
  render(<BrandingProvider><Probe /></BrandingProvider>);
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("branding provider", () => {
  it("renders the defaults until the branding arrives, then the configured values", async () => {
    let arrive!: (branding: PublicBranding) => void;
    calls.getBranding.mockReturnValue(new Promise((resolve) => { arrive = resolve; }));
    mount();
    expect(screen.getByText("name: Felis")).toBeTruthy();
    await act(async () => arrive(acme));
    expect(screen.getByText("name: Acme Cloud")).toBeTruthy();
  });

  it("keeps the defaults when the branding cannot be read", async () => {
    calls.getBranding.mockRejectedValue({ status: 500, code: "internal", message: "down" });
    mount();
    await waitFor(() => expect(calls.getBranding).toHaveBeenCalledOnce());
    expect(screen.getByText("name: Felis")).toBeTruthy();
  });

  it("reads the branding again when asked, so a saved change shows at once", async () => {
    calls.getBranding.mockResolvedValueOnce(DEFAULT_BRANDING).mockResolvedValueOnce(acme);
    mount();
    await waitFor(() => expect(calls.getBranding).toHaveBeenCalledOnce());
    await userEvent.click(screen.getByRole("button", { name: "reread" }));
    expect(await screen.findByText("name: Acme Cloud")).toBeTruthy();
  });
});
