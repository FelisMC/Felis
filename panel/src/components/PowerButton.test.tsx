// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { PowerButton } from "./PowerButton";

const { wake, stop } = vi.hoisted(() => ({ wake: vi.fn(), stop: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, wake, stop } };
});

const t = (key: string, opts?: Record<string, unknown>) => i18next.t(key, opts);

beforeEach(() => {
  wake.mockReset();
  stop.mockReset();
});

describe("PowerButton", () => {
  it("wakes a stopped server and tells the parent", async () => {
    wake.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" live={false} onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:wake") }));

    expect(wake).toHaveBeenCalledWith("lobby");
    expect(onChanged).toHaveBeenCalledOnce();
  });

  it("stays on the page with the reason when the wake is refused", async () => {
    wake.mockRejectedValue({ status: 429, code: "quota_exceeded", message: "raw" });
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" live={false} onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:wake") }));

    expect(await screen.findByRole("alert")).toHaveProperty("textContent", t("errors:quota_exceeded"));
    expect(onChanged).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: t("servers:wake") })).toHaveProperty("disabled", false);
  });

  it("stops an empty server without asking", async () => {
    stop.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" live playersOnline={0} onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));

    expect(stop).toHaveBeenCalledWith("lobby");
    expect(onChanged).toHaveBeenCalledOnce();
  });

  it("asks before disconnecting players, and cancel sends nothing", async () => {
    render(<PowerButton name="lobby" live playersOnline={3} onChanged={vi.fn()} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));
    expect(screen.getByText(t("servers:stop_confirm_players", { count: 3 }))).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: t("common:cancel") }));

    expect(stop).not.toHaveBeenCalled();
    expect(screen.queryByText(t("servers:stop_confirm_players", { count: 3 }))).toBeNull();
  });

  it("asks when the player count cannot be read", async () => {
    stop.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" live playersOnline={0} playerCountUnknown onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));
    expect(stop).not.toHaveBeenCalled();
    expect(screen.getByText(t("servers:stop_confirm_unknown"))).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));

    expect(stop).toHaveBeenCalledWith("lobby");
    expect(onChanged).toHaveBeenCalledOnce();
  });

  it("keeps the confirmation open with the reason when the stop fails", async () => {
    stop.mockRejectedValue({ status: 409, code: "cooldown", message: "raw" });
    const onChanged = vi.fn();
    render(<PowerButton name="lobby" live playersOnline={2} onChanged={onChanged} />);

    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));
    await userEvent.click(screen.getByRole("button", { name: t("servers:stop") }));

    expect(await screen.findByText(t("errors:cooldown"))).toBeTruthy();
    expect(screen.getByText(t("servers:stop_confirm_players", { count: 2 }))).toBeTruthy();
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("sends one call however fast it is clicked", async () => {
    let resolve!: () => void;
    wake.mockReturnValue(new Promise<void>((r) => (resolve = r)));
    render(<PowerButton name="lobby" live={false} onChanged={vi.fn()} />);

    const button = screen.getByRole("button", { name: t("servers:wake") });
    await userEvent.click(button);
    await userEvent.click(screen.getByRole("button", { name: t("servers:waking") }));
    resolve();

    expect(wake).toHaveBeenCalledOnce();
  });
});
