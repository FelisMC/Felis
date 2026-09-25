// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";
import { useState } from "react";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { ConfirmDialog } from "./ConfirmDialog";

function Harness({ onConfirm }: { onConfirm: () => Promise<void> }) {
  const [open, setOpen] = useState(true);
  return (
    <>
      <span>{open ? "open" : "closed"}</span>
      <button onClick={() => setOpen(true)}>reopen</button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="Remove it?"
        description="It cannot come back."
        confirmLabel="Remove"
        onConfirm={onConfirm}
      />
    </>
  );
}

describe("ConfirmDialog", () => {
  it("closes once the action succeeds", async () => {
    const action = vi.fn().mockResolvedValue(undefined);
    render(<Harness onConfirm={action} />);

    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Remove" }));
    expect(action).toHaveBeenCalledTimes(1);
    expect(await screen.findByText("closed")).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("keeps the dialog open with the reason when the action fails, and lets it be retried", async () => {
    const action = vi
      .fn()
      .mockRejectedValueOnce({ status: 409, code: "test", message: "still in use" })
      .mockResolvedValueOnce(undefined);
    render(<Harness onConfirm={action} />);

    const dialog = screen.getByRole("dialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe("still in use");
    expect(screen.getByText("open")).toBeTruthy();

    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("closed")).toBeTruthy();
    expect(action).toHaveBeenCalledTimes(2);
  });

  it("cannot be dismissed while the action runs, and clears the old failure on retry", async () => {
    let finish!: (v?: unknown) => void;
    const action = vi
      .fn()
      .mockRejectedValueOnce({ status: 409, code: "test", message: "still in use" })
      .mockImplementationOnce(() => new Promise((r) => (finish = r)));
    render(<Harness onConfirm={action} />);
    const dialog = screen.getByRole("dialog");
    const cancel = within(dialog).getByRole("button", { name: i18next.t("common:cancel") });

    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await within(dialog).findByRole("alert");
    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    expect(within(dialog).queryByRole("alert")).toBeNull();
    await userEvent.keyboard("{Escape}");
    await userEvent.click(cancel, { pointerEventsCheck: 0 });
    expect(screen.getByText("open")).toBeTruthy();

    finish();
    expect(await screen.findByText("closed")).toBeTruthy();
  });

  it("forgets an old failure when reopened", async () => {
    const action = vi.fn().mockRejectedValue({ status: 409, code: "test", message: "still in use" });
    render(<Harness onConfirm={action} />);
    const dialog = screen.getByRole("dialog");

    await userEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await within(dialog).findByRole("alert");
    await userEvent.click(within(dialog).getByRole("button", { name: i18next.t("common:cancel") }));
    expect(await screen.findByText("closed")).toBeTruthy();

    await userEvent.click(screen.getByRole("button", { name: "reopen" }));
    expect(within(await screen.findByRole("dialog")).queryByRole("alert")).toBeNull();
  });
});
