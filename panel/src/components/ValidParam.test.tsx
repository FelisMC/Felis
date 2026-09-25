// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";
import { ValidParam } from "./ValidParam";

// A page with state of its own, as a console's half-typed command is.
function Page() {
  return (
    <>
      <input aria-label="command" />
      <Link to="/servers/beta">beta</Link>
    </>
  );
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="servers/:name" element={<ValidParam param="name" pattern={/^[a-z]+$/} />}>
          <Route index element={<Page />} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

describe("ValidParam", () => {
  it("starts the page afresh when the parameter changes", () => {
    renderAt("/servers/alpha");
    fireEvent.change(screen.getByLabelText("command"), { target: { value: "stop" } });

    fireEvent.click(screen.getByRole("link", { name: "beta" }));
    expect((screen.getByLabelText("command") as HTMLInputElement).value).toBe("");
  });

  it("says not found for a parameter of the wrong shape", () => {
    renderAt("/servers/Alpha..");
    expect(screen.queryByLabelText("command")).toBeNull();
    expect(screen.getByText("Page not found")).toBeTruthy();
  });
});
