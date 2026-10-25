// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { ExtraPortsCard } from "./ExtraPortsCard";

const t = (key: string, options: Record<string, string | number> = {}) => i18next.t(`servers:${key}`, options);
afterEach(() => vi.restoreAllMocks());

describe("ExtraPortsCard", () => {
  it("copies the complete direct address alongside protocol and both ports", async () => {
    const user = userEvent.setup();
    const write = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue(undefined);
    const address = "[2001:db8::10]:31001";
    render(<ExtraPortsCard ports={[{ protocol: "UDP", port: 24454, nodePort: 31001, address }]} />);
    expect(screen.getByText(t("extra_ports_mapping", { protocol: "UDP", port: 24454 }))).toBeTruthy();
    expect(screen.getByText(t("extra_ports_external", { port: 31001 }))).toBeTruthy();
    await user.click(screen.getByRole("button", { name: i18next.t("common:copy_address", { address }) }));
    expect(write).toHaveBeenCalledExactlyOnceWith(address);
    expect(screen.getByText(i18next.t("common:address_copied"))).toBeTruthy();
    expect(screen.getByText(t("extra_ports_direct_hint"))).toBeTruthy();
  });

  it("shows unassigned addresses and applying status without inventing a direct address", () => {
    render(<ExtraPortsCard ports={[{ protocol: "TCP", port: 8123 }]} pending />);
    expect(screen.getByText(t("extra_ports_pending"))).toBeTruthy();
    expect(screen.getByText(t("extra_ports_address_unavailable"))).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("shows an assigned external port when its direct address is unavailable", () => {
    render(<ExtraPortsCard ports={[{ protocol: "TCP", port: 8123, nodePort: 31002 }]} />);
    expect(screen.getByText(t("extra_ports_external", { port: 31002 }))).toBeTruthy();
    expect(screen.getByText(t("extra_ports_address_unavailable"))).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("retains the applying message while a cleared list is being applied", () => {
    render(<ExtraPortsCard ports={[]} pending />);
    expect(screen.getByText(t("extra_ports_pending"))).toBeTruthy();
    expect(screen.getByText(t("extra_ports_empty"))).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });
});
