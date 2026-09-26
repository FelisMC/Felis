// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { RetireCard, RetireNotice } from "./Retirement";

const calls = vi.hoisted(() => ({ retireServer: vi.fn(), cancelRetire: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string, opts?: Record<string, unknown>) => i18next.t(key, opts);
const hourAgo = () => new Date(Date.now() - 3600_000).toISOString();

beforeEach(() => {
  calls.retireServer.mockReset();
  calls.cancelRetire.mockReset();
});

describe("RetireCard", () => {
  it("lets an owner give the server up once its name is typed out, and never offers a deletion", async () => {
    calls.retireServer.mockResolvedValue({ name: "survival", retiring: { requested_at: hourAgo(), delete: false } });
    const onChanged = vi.fn();
    render(<RetireCard name="survival" label="Survival World" isAdmin={false} onChanged={onChanged} />);

    expect(screen.queryByRole("button", { name: t("servers:retire_delete") })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: t("servers:retire_release") }));

    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).toContain(t("servers:retire_release_title", { name: "Survival World" }));
    expect(dialog.textContent).toContain(t("servers:retire_step_cancel_owner"));
    const confirm = screen.getByRole("button", { name: t("servers:retire_confirm_release") });
    const input = screen.getByLabelText(t("servers:retire_confirm_label"), { exact: false });

    // The display name, a different case or a prefix is not the server's name.
    for (const wrong of ["Survival World", "Survival", "surviva"]) {
      await userEvent.clear(input);
      await userEvent.type(input, wrong + "{Enter}");
      expect(confirm).toHaveProperty("disabled", true);
    }
    expect(calls.retireServer).not.toHaveBeenCalled();

    await userEvent.clear(input);
    await userEvent.type(input, "survival");
    expect(confirm).toHaveProperty("disabled", false);
    await userEvent.click(confirm);

    expect(calls.retireServer).toHaveBeenCalledExactlyOnceWith("survival", { confirm: "survival", delete: false });
    expect(onChanged).toHaveBeenCalledOnce();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("lets an admin delete the server, and says only an admin can take that back", async () => {
    calls.retireServer.mockResolvedValue({ name: "survival", retiring: { requested_at: hourAgo(), delete: true } });
    const onChanged = vi.fn();
    render(<RetireCard name="survival" label="survival" isAdmin onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:retire_delete") }));
    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).toContain(t("servers:retire_step_delete"));
    expect(dialog.textContent).toContain(t("servers:retire_step_cancel_admin"));

    await userEvent.type(screen.getByLabelText(t("servers:retire_confirm_label"), { exact: false }), "survival{Enter}");

    expect(calls.retireServer).toHaveBeenCalledExactlyOnceWith("survival", { confirm: "survival", delete: true });
    expect(onChanged).toHaveBeenCalledOnce();
  });

  it("keeps the dialog open with the reason when the request is refused", async () => {
    calls.retireServer.mockRejectedValue({ status: 409, code: "world_volume_orphaned", message: "raw" });
    const onChanged = vi.fn();
    render(<RetireCard name="survival" label="survival" isAdmin onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:retire_delete") }));
    await userEvent.type(screen.getByLabelText(t("servers:retire_confirm_label"), { exact: false }), "survival");
    await userEvent.click(screen.getByRole("button", { name: t("servers:retire_confirm_delete") }));

    expect((await screen.findByRole("alert")).textContent).toBe(t("errors:world_volume_orphaned"));
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(onChanged).not.toHaveBeenCalled();
  });
});

describe("RetireNotice", () => {
  it("lets the owner take back giving the server up", async () => {
    calls.cancelRetire.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(
      <RetireNotice name="survival" retiring={{ requested_at: hourAgo(), delete: false }} isAdmin={false} onChanged={onChanged} />,
    );

    expect(screen.getByRole("status").textContent).toContain(t("servers:retiring_title_release"));
    expect(screen.getByRole("status").textContent).toContain("1 hour ago");
    await userEvent.click(screen.getByRole("button", { name: t("servers:retiring_cancel") }));

    expect(calls.cancelRetire).toHaveBeenCalledExactlyOnceWith("survival");
    expect(onChanged).toHaveBeenCalledOnce();
  });

  it("offers the owner no cancel of an admin's deletion, and says who can", () => {
    render(
      <RetireNotice name="survival" retiring={{ requested_at: hourAgo(), delete: true }} isAdmin={false} onChanged={vi.fn()} />,
    );

    expect(screen.getByRole("status").textContent).toContain(t("servers:retiring_title_delete"));
    expect(screen.getByRole("status").textContent).toContain(t("servers:retiring_cancel_admin_only"));
    expect(screen.queryByRole("button", { name: t("servers:retiring_cancel") })).toBeNull();
  });

  it("lets an admin cancel a deletion, and shows why a cancel failed", async () => {
    calls.cancelRetire.mockRejectedValue({ status: 403, code: "forbidden", message: "raw" });
    const onChanged = vi.fn();
    render(
      <RetireNotice name="survival" retiring={{ requested_at: hourAgo(), delete: true }} isAdmin onChanged={onChanged} />,
    );

    expect(screen.getByRole("status").textContent).not.toContain(t("servers:retiring_cancel_admin_only"));
    await userEvent.click(screen.getByRole("button", { name: t("servers:retiring_cancel") }));

    expect(calls.cancelRetire).toHaveBeenCalledWith("survival");
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(onChanged).not.toHaveBeenCalled();
  });
});
