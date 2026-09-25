// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { SETUP_REQUIRED_EVENT } from "@/lib/api";
import { SetupRequiredRedirect } from "./SetupRequiredRedirect";

function Tree({ listening }: { listening: boolean }) {
  return (
    <MemoryRouter initialEntries={["/servers"]}>
      {listening && <SetupRequiredRedirect />}
      <Routes>
        <Route path="/setup" element={<p>setup wizard</p>} />
        <Route path="/servers" element={<p>servers</p>} />
      </Routes>
    </MemoryRouter>
  );
}

const announce = () =>
  act(() => {
    window.dispatchEvent(new Event(SETUP_REQUIRED_EVENT));
  });

describe("SetupRequiredRedirect", () => {
  it("routes to the wizard when a call answers 403 setup_required", () => {
    render(<Tree listening />);
    expect(screen.getByText("servers")).toBeTruthy();

    announce();

    expect(screen.getByText("setup wizard")).toBeTruthy();
  });

  it("stops redirecting once it is unmounted", () => {
    const view = render(<Tree listening />);
    view.rerender(<Tree listening={false} />);

    announce();

    expect(screen.getByText("servers")).toBeTruthy();
  });
});
