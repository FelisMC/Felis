// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { InlineError, MessageLine } from "./MessageLine";
import { ErrorState } from "./States";
import { FeedbackLine } from "./players/shared";

// A failure has to interrupt a screen reader (role=alert); a success waits for
// a pause (role=status). Neither may be an empty region.
describe("announced messages", () => {
  it("MessageLine interrupts for an error and waits for a success", () => {
    const { rerender } = render(<MessageLine kind="error" message="save failed" />);
    expect(screen.getByRole("alert").textContent).toBe("save failed");
    expect(screen.queryByRole("status")).toBeNull();

    rerender(<MessageLine kind="success" message="saved" />);
    expect(screen.getByRole("status").textContent).toBe("saved");
    expect(screen.queryByRole("alert")).toBeNull();

    rerender(<MessageLine kind="error" message="short" compact />);
    expect(screen.getByRole("alert").textContent).toBe("short");
  });

  it("InlineError renders nothing until there is an error", () => {
    const { rerender, container } = render(<InlineError message={null} />);
    expect(container.innerHTML).toBe("");

    rerender(<InlineError message="" />);
    expect(container.innerHTML).toBe("");

    rerender(<InlineError message="wrong code" />);
    expect(screen.getByRole("alert").textContent).toBe("wrong code");
  });

  it("FeedbackLine interrupts only for a failed player action", () => {
    const { rerender, container } = render(<FeedbackLine fb={{ kind: "err", msg: "no such player" }} />);
    expect(screen.getByRole("alert").textContent).toBe("no such player");

    rerender(<FeedbackLine fb={{ kind: "ok", msg: "whitelisted" }} />);
    expect(screen.getByRole("status").textContent).toBe("whitelisted");
    expect(screen.queryByRole("alert")).toBeNull();

    rerender(<FeedbackLine fb={null} />);
    expect(container.innerHTML).toBe("");
  });

  it("ErrorState reads out the reason, and only the reason", () => {
    render(<ErrorState error={{ status: 409, code: "test", message: "backend refused" }} onRetry={() => {}} />);
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toBe("backend refused");
    expect(alert.querySelector("button")).toBeNull();
  });
});
