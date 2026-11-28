// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { PlatformModeCard } from "./PlatformModeCard";
import { FeaturesContext } from "@/lib/features";
import type { PlatformFeaturesSettings } from "@/lib/types";

const calls = vi.hoisted(() => ({
  getFeatures: vi.fn(),
  setFeatures: vi.fn(),
}));

vi.mock("@/lib/api", async (original) => ({
  ...(await original<typeof import("@/lib/api")>()),
  api: { ...(await original<typeof import("@/lib/api")>()).api, ...calls },
}));

const t = (key: string) => i18next.t(`admin:${key}`);

const INITIAL: PlatformFeaturesSettings = {
  mode: "team",
  useLobby: false,
  lobbyAvailable: true,
  customImages: false,
  allowSubmissions: false,
  requireAccountLink: false,
  revision: "rev-1",
};

beforeEach(() => {
  vi.clearAllMocks();
  calls.getFeatures.mockResolvedValue(INITIAL);
  calls.setFeatures.mockImplementation(async (body) => ({
    mode: body.mode,
    useLobby: body.useLobby,
    lobbyAvailable: true,
    customImages: body.customImages,
    allowSubmissions: body.allowSubmissions,
    revision: "rev-2",
  }));
});

function renderCard(apply = vi.fn(), onApplied = vi.fn()) {
  render(
    <FeaturesContext.Provider value={{ features: INITIAL, loaded: true, apply, reload: vi.fn() }}>
      <PlatformModeCard onApplied={onApplied} />
    </FeaturesContext.Provider>,
  );
  return { apply, onApplied };
}

describe("PlatformModeCard", () => {
  it("saves component switches directly without opening the mode confirmation dialog", async () => {
    const user = userEvent.setup();
    const { apply, onApplied } = renderCard();
    const imagesSwitch = await screen.findByRole("switch", { name: t("features_custom_images_label") });
    await waitFor(() => expect((imagesSwitch as HTMLButtonElement).disabled).toBe(false));

    await user.click(imagesSwitch);
    await user.click(screen.getByRole("button", { name: t("features_save") }));

    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() =>
      expect(calls.setFeatures).toHaveBeenCalledWith({
        mode: "team",
        useLobby: false,
        customImages: true,
        allowSubmissions: false,
        syncRequireAccountLink: undefined,
        revision: "rev-1",
      }),
    );
    expect(apply).toHaveBeenCalledTimes(1);
    expect(onApplied).toHaveBeenCalledTimes(1);
  });

  it("opens the confirmation dialog on mode switch and forwards sync choices", async () => {
    const user = userEvent.setup();
    calls.getFeatures.mockResolvedValue({ ...INITIAL, customImages: true });
    renderCard();

    const communityBtn = await screen.findByRole("button", { name: new RegExp(t("features_mode_community")) });
    await waitFor(() => expect((communityBtn as HTMLButtonElement).disabled).toBe(false));
    await user.click(communityBtn);
    await user.click(screen.getByRole("button", { name: t("features_save") }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(t("features_switch_title_community"))).toBeTruthy();
    const syncLink = within(dialog).getByRole("checkbox", { name: new RegExp(t("features_switch_sync_link_community")) }) as HTMLInputElement;
    const syncSubs = within(dialog).getByRole("checkbox", { name: new RegExp(t("features_switch_sync_submissions_community")) }) as HTMLInputElement;
    expect(syncLink.checked).toBe(true);
    expect(syncSubs.checked).toBe(true);

    await user.click(syncLink);
    await user.click(within(dialog).getByRole("button", { name: t("features_switch_confirm") }));

    await waitFor(() =>
      expect(calls.setFeatures).toHaveBeenCalledWith({
        mode: "community",
        useLobby: false,
        customImages: true,
        allowSubmissions: true,
        syncRequireAccountLink: false,
        revision: "rev-1",
      }),
    );
  });

  it("disables the lobby toggle when lobby is not deployed", async () => {
    calls.getFeatures.mockResolvedValue({ ...INITIAL, lobbyAvailable: false });
    renderCard();
    const lobbySwitch = await screen.findByRole("switch", { name: t("features_lobby_label") });
    await waitFor(() => expect(screen.getByText(t("features_lobby_unavailable"))).toBeTruthy());
    expect((lobbySwitch as HTMLButtonElement).disabled).toBe(true);
  });
});
