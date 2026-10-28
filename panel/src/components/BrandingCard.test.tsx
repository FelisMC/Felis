// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import { BrandingCard } from "./BrandingCard";
import type { BrandingSettings } from "@/lib/types";

const calls = vi.hoisted(() => ({
  getBrandingSettings: vi.fn(),
  setBrandingSettings: vi.fn(),
  uploadBrandingImage: vi.fn(),
  removeBrandingImage: vi.fn(),
}));
vi.mock("@/lib/api", async (original) => {
  const actual = await original<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string, options?: Record<string, unknown>) => i18next.t(key, options);
const unset: BrandingSettings = { name: "", notice: "", show_powered_by: true, has_logo: false, has_background: false, revision: "rev-0" };

beforeEach(() => {
  vi.clearAllMocks();
  calls.getBrandingSettings.mockResolvedValue(unset);
});

async function show() {
  render(<BrandingCard />);
  const name = (await screen.findByLabelText(t("admin:branding_name_label"))) as HTMLInputElement;
  await waitFor(() => expect(name.disabled).toBe(false));
  return {
    name,
    notice: screen.getByLabelText(t("admin:branding_notice_label")) as HTMLTextAreaElement,
    save: screen.getByRole("button", { name: t("admin:branding_save") }) as HTMLButtonElement,
  };
}

describe("branding settings", () => {
  it("saves the edited fields with the revision that was read and keeps the draft after a conflict", async () => {
    const user = userEvent.setup();
    const { name, notice, save } = await show();
    expect(save.disabled).toBe(true);
    await user.type(name, "Acme Cloud");
    await user.type(notice, "Back at 9");
    await user.click(screen.getByRole("switch", { name: t("admin:branding_powered_by_label") }));
    calls.setBrandingSettings.mockRejectedValueOnce({ status: 409, code: "conflict", message: "changed" });
    await user.click(save);
    await screen.findByText(t("errors:conflict"));
    expect(name.value).toBe("Acme Cloud");

    calls.setBrandingSettings.mockImplementation(async (body) => ({ ...body, has_logo: false, has_background: false, revision: "rev-1" }));
    await user.click(save);
    await screen.findByText(t("admin:branding_saved"));
    expect(calls.setBrandingSettings).toHaveBeenLastCalledWith({ name: "Acme Cloud", notice: "Back at 9", show_powered_by: false, revision: "rev-0" });
    expect(save.disabled).toBe(true);
  });

  it.each([
    ["a name over 40 characters", "name", "名".repeat(41), "admin:branding_name_invalid", {}],
    ["a notice over 1000 characters", "notice", "a".repeat(1001), "admin:branding_notice_over", { n: 1 }],
    ["a notice with a control character", "notice", "a\u0007b", "admin:branding_notice_control", {}],
  ] as const)("refuses %s before sending anything", async (_label, field, value, problem, options) => {
    const fields = await show();
    fireEvent.change(fields[field], { target: { value } });
    expect(screen.getByText(t(problem, options))).toBeTruthy();
    expect(fields.save.disabled).toBe(true);
    expect(calls.setBrandingSettings).not.toHaveBeenCalled();
  });

  it("refuses an image of the wrong type or over its size without sending it", async () => {
    const user = userEvent.setup({ applyAccept: false });
    await show();
    const logo = screen.getByLabelText(t("admin:branding_logo_upload"));
    await user.upload(logo, new File(["gif"], "logo.gif", { type: "image/gif" }));
    expect((await screen.findByRole("alert")).textContent).toBe(t("admin:branding_image_type"));
    await user.upload(logo, new File([new Uint8Array(257 << 10)], "logo.png", { type: "image/png" }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toBe(t("admin:branding_logo_too_large")));
    await user.upload(screen.getByLabelText(t("admin:branding_background_upload")), new File([new Uint8Array((2 << 20) + 1)], "back.webp", { type: "image/webp" }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toBe(t("admin:branding_background_too_large")));
    expect(calls.uploadBrandingImage).not.toHaveBeenCalled();
  });

  it("uploads a logo and removes it again", async () => {
    const user = userEvent.setup();
    await show();
    const file = new File(["png"], "logo.png", { type: "image/png" });
    calls.uploadBrandingImage.mockResolvedValue({ ...unset, has_logo: true });
    await user.upload(screen.getByLabelText(t("admin:branding_logo_upload")), file);
    await screen.findByText(t("admin:branding_image_uploaded"));
    expect(calls.uploadBrandingImage).toHaveBeenCalledWith("logo", file);

    calls.removeBrandingImage.mockResolvedValue(unset);
    await user.click(screen.getByRole("button", { name: t("admin:branding_logo_remove") }));
    await screen.findByText(t("admin:branding_image_removed"));
    expect(calls.removeBrandingImage).toHaveBeenCalledWith("logo");
    expect(screen.queryByRole("button", { name: t("admin:branding_logo_remove") })).toBeNull();
  });
});
