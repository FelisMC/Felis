// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { APITokenSettingsCard } from "./APITokenSettingsCard";

const calls = vi.hoisted(() => ({ getAPITokenSettings: vi.fn(), setAPITokenSettings: vi.fn() }));
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string) => i18next.t(key);

beforeEach(() => {
  vi.clearAllMocks();
  calls.getAPITokenSettings.mockResolvedValue({ enabled: false, revision: "rev-0" });
  calls.setAPITokenSettings.mockResolvedValue({ enabled: true, revision: "rev-1" });
});

describe("API key setting", () => {
  it("saves the switch together with the revision it was read at", async () => {
    const user = userEvent.setup();
    render(<APITokenSettingsCard />);
    const toggle = (await screen.findByRole("switch", { name: new RegExp(t("admin:api_tokens_enabled")) })) as HTMLButtonElement;
    await waitFor(() => expect(toggle.disabled).toBe(false));
    const save = screen.getByRole("button", { name: t("admin:api_tokens_save") }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);

    await user.click(toggle);
    expect(save.disabled).toBe(false);
    await user.click(save);

    await waitFor(() => expect(calls.setAPITokenSettings).toHaveBeenCalledWith({ enabled: true, revision: "rev-0" }));
    expect(await screen.findByText(t("admin:api_tokens_saved"))).toBeTruthy();
    expect(save.disabled).toBe(true);
  });
});
