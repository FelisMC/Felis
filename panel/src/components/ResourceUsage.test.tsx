// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import i18next from "i18next";
import { DiskUsage, ResourceUsage } from "./ResourceUsage";

const mocks = vi.hoisted(() => ({ serverUsage: vi.fn() }));
vi.mock("@/lib/api", async (original) => ({ ...await original<typeof import("@/lib/api")>(), api: mocks }));
const now = "2026-10-23T00:00:00Z";
const sample = { at: now, cpuMilli: 500, memoryBytes: 512 * 1024 ** 2 };
const disk = { usedBytes: 9 * 1024 ** 3, limitBytes: 10 * 1024 ** 3, sampledAt: "2026-10-22T23:59:00Z", stale: false };
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
    render(<ResourceUsage name="survival" running cpu="2" memory="2Gi" disk={disk} />);
    await screen.findByText("25.0%");
    mocks.serverUsage.mockRejectedValue(new Error("metrics offline"));
    fireEvent(document, new Event("visibilitychange"));
    await waitFor(() => expect(screen.queryByText("25.0%")).toBeNull());
    expect(screen.getByText(t("usage_unavailable"))).toBeTruthy();
    expect(screen.getAllByRole("img")).toHaveLength(2);
    expect(screen.getByRole("meter", { name: t("disk_title") }).getAttribute("aria-valuetext")).toBe("9.0 GiB / 10 GiB");
  });
  it("does not report old samples as current usage", async () => {
    mocks.serverUsage.mockResolvedValue({ current: { ...sample, at: "2026-10-22T23:59:00Z" }, history: [sample] });
    render(<ResourceUsage name="survival" running cpu="2" memory="2Gi" />);
    await waitFor(() => expect(mocks.serverUsage).toHaveBeenCalled());
    expect(screen.queryByText("25.0%")).toBeNull();
    expect(screen.getByText(t("usage_unavailable"))).toBeTruthy();
  });
  it("keeps history while stopped without showing the last value as current", async () => {
    render(<ResourceUsage name="survival" running={false} cpu="2" memory="2Gi" disk={disk} />);
    expect(await screen.findByText(t("usage_stopped"))).toBeTruthy();
    expect(screen.queryByText("25.0%")).toBeNull();
    expect(screen.getAllByRole("img")).toHaveLength(2);
    expect(screen.getByText("9.0 GiB / 10 GiB")).toBeTruthy();
    expect(screen.getByRole("meter").getAttribute("aria-valuenow")).toBe(String(disk.usedBytes));
  });
});

describe("DiskUsage", () => {
  it.each([false, true])("does not present an unknown sample as zero (full=%s)", (diskFull) => {
    render(<DiskUsage diskFull={diskFull} />);
    expect(screen.getByText(t("disk_unknown"))).toBeTruthy();
    expect(screen.queryByRole("meter")).toBeNull();
    expect(screen.queryByText(t("disk_full_blocked")) !== null).toBe(diskFull);
  });

  it.each([899, 900])("warns from 90 percent without blocking startup (%s/1000)", (usedBytes) => {
    render(<DiskUsage disk={{ ...disk, usedBytes, limitBytes: 1000 }} />);
    expect(screen.queryByText(t("disk_warning")) !== null).toBe(usedBytes === 900);
    expect(screen.queryByText(t("disk_full_blocked"))).toBeNull();
  });

  it("keeps the backend full restriction when a stale sample is below the quota", () => {
    const { container, rerender } = render(<DiskUsage disk={{ ...disk, usedBytes: 100, limitBytes: 1000, stale: true }} diskFull />);
    expect(screen.getByText(t("disk_stale"), { exact: false })).toBeTruthy();
    expect(container.querySelector("time")?.dateTime).toBe(disk.sampledAt);
    expect(screen.getByText(t("disk_full_blocked"))).toBeTruthy();
    rerender(<DiskUsage disk={{ ...disk, usedBytes: 100, limitBytes: 1000 }} diskFull={false} />);
    expect(screen.queryByText(t("disk_full_blocked"))).toBeNull();
    expect(screen.queryByText(t("disk_stale"), { exact: false })).toBeNull();
  });

  it("reports actual usage above the quota while keeping the meter in range", () => {
    render(<DiskUsage disk={{ ...disk, usedBytes: 1200, limitBytes: 1000 }} diskFull />);
    const meter = screen.getByRole("meter", { name: t("disk_title") });
    expect(screen.getByText("1.2 KiB / 1000 B")).toBeTruthy();
    expect(meter.getAttribute("aria-valuenow")).toBe("1000");
    expect(meter.getAttribute("aria-valuetext")).toBe("1.2 KiB / 1000 B");
  });
});
