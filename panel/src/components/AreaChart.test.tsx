// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { noteServerDate } from "@/lib/clock";
import { AreaChart } from "./AreaChart";

describe("AreaChart", () => {
  it("draws the server's last half hour when the server clock runs two hours behind the browser", () => {
    const server = Date.now() - 2 * 3600_000;
    noteServerDate(new Date(server).toUTCString());
    const points = Array.from({ length: 120 }, (_, i) => ({ at: new Date(server - (119 - i) * 15_000).toISOString(), value: 100 + i }));
    render(<AreaChart points={points} label="CPU" />);
    expect(screen.queryByText("Collecting data")).toBeNull();
    expect(screen.getByText("30 minutes ago")).toBeTruthy();
  });
});
