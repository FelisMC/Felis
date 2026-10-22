// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import i18next from "i18next";
import { ResourceUsage } from "./ResourceUsage";

const mocks = vi.hoisted(() => ({ serverUsage: vi.fn() }));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: mocks }));
const now = "2026-10-23T00:00:00Z";
const sample = { at: now, cpuMilli: 500, memoryBytes: 512 * 1024 ** 2 };
const t = (key: string) => i18next.t(`servers:${key}`);
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(now));
  mocks.serverUsage.mockReset();
  mocks.serverUsage.mockResolvedValue({ current: sample, history: [sample] });
});
afterEach(() => vi.useRealTimers());

describe("ResourceUsage", () => {
  it("shows limit-relative CPU, working-set memory and named charts", async () => {
    render(<ResourceUsage name="survival" running cpu="2" memory="2Gi" />);
    expect(await screen.findByText("25.0%")).toBeTruthy();
    expect(screen.getByText("512 MiB / 2.0 GiB")).toBeTruthy();
    expect(screen.getByRole("img", { name: t("usage_cpu_curve") })).toBeTruthy();
    expect(screen.getByRole("img", { name: t("usage_memory_curve") })).toBeTruthy();
    expect(mocks.serverUsage).toHaveBeenCalledWith("survival");
  });
  it("hides old values when refresh fails and retains the charts", async () => {
    render(<ResourceUsage name="survival" running cpu="2" memory="2Gi" />);
    await screen.findByText("25.0%");
    mocks.serverUsage.mockRejectedValue(new Error("metrics offline"));
    fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(screen.queryByText("25.0%")).toBeNull());
    expect(screen.getByText(t("usage_unavailable"))).toBeTruthy();
    expect(screen.getAllByRole("img")).toHaveLength(2);
  });
  it("does not report old samples as current usage", async () => {
    mocks.serverUsage.mockResolvedValue({ current: { ...sample, at: "2026-10-22T23:59:00Z" }, history: [sample] });
    render(<ResourceUsage name="survival" running cpu="2" memory="2Gi" />);
    await waitFor(() => expect(mocks.serverUsage).toHaveBeenCalled());
    expect(screen.queryByText("25.0%")).toBeNull();
    expect(screen.getByText(t("usage_unavailable"))).toBeTruthy();
  });
  it("keeps history while stopped without showing the last value as current", async () => {
    render(<ResourceUsage name="survival" running={false} cpu="2" memory="2Gi" />);
    expect(await screen.findByText(t("usage_stopped"))).toBeTruthy();
    expect(screen.queryByText("25.0%")).toBeNull();
    expect(screen.getAllByRole("img")).toHaveLength(2);
  });
});
