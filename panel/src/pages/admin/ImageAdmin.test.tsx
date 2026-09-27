// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { ImageAdmin } from "./ImageAdmin";

const calls = vi.hoisted(() => ({ listImages: vi.fn(), removeImage: vi.fn(), listBuilds: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string) => i18next.t(key);
const REF = "registry.felis.svc:5000/paper:1.21";

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  calls.listImages.mockResolvedValue([{ image_ref: REF, source: "external", enabled: true }]);
  calls.listBuilds.mockResolvedValue({ builds: [], total: 0 });
  // A native dialog here would block (or be blocked in an embedded browser).
  vi.spyOn(window, "confirm").mockImplementation(() => {
    throw new Error("window.confirm used");
  });
  vi.spyOn(window, "alert").mockImplementation(() => {
    throw new Error("window.alert used");
  });
});
afterEach(() => vi.restoreAllMocks());

async function openRemove() {
  render(
    <MemoryRouter>
      <ImageAdmin />
    </MemoryRouter>,
  );
  await userEvent.click(await screen.findByRole("button", { name: t("admin:delete_image_tooltip") }));
  return screen.getByRole("dialog", { name: t("admin:delete_image_title") });
}

describe("ImageAdmin removal", () => {
  it("asks in a dialog that names the image, then removes it", async () => {
    calls.removeImage.mockResolvedValue(undefined);
    const dialog = await openRemove();
    expect(within(dialog).getByText(REF)).toBeTruthy();
    expect(calls.removeImage).not.toHaveBeenCalled();

    await userEvent.click(within(dialog).getByRole("button", { name: t("admin:delete_image_confirm") }));
    expect(calls.removeImage).toHaveBeenCalledWith(REF);
    await vi.waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(calls.listImages).toHaveBeenCalledTimes(2);
  });

  it("shows a failed removal in the dialog", async () => {
    calls.removeImage.mockRejectedValue({ status: 409, code: "test", message: "a server still uses it" });
    const dialog = await openRemove();

    await userEvent.click(within(dialog).getByRole("button", { name: t("admin:delete_image_confirm") }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe("a server still uses it");
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("leaves the image alone when the dialog is dismissed", async () => {
    const dialog = await openRemove();
    await userEvent.click(within(dialog).getByRole("button", { name: t("common:cancel") }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(calls.removeImage).not.toHaveBeenCalled();
  });
});

describe("ImageAdmin source", () => {
  afterEach(() => i18next.changeLanguage("en-US"));

  it("names the sources Felis writes in the UI language, finds them by that name, and shows others as stored", async () => {
    calls.listImages.mockResolvedValue([
      { image_ref: "registry.felis.svc:5000/paper:demo", source: "recommended", enabled: true },
      { image_ref: "docker.io/itzg/minecraft-server:java21", source: "external", enabled: true },
      { image_ref: "mirror.example.test/forge:1.20", source: "community-mirror", enabled: true },
    ]);
    await i18next.changeLanguage("zh-CN");
    render(
      <MemoryRouter>
        <ImageAdmin />
      </MemoryRouter>,
    );

    expect(await screen.findByText("推荐")).toBeTruthy();
    expect(screen.getByText("外部")).toBeTruthy();
    expect(screen.getByText("community-mirror")).toBeTruthy();
    expect(screen.queryByText("recommended")).toBeNull();

    await userEvent.type(screen.getByRole("textbox"), "推荐");
    await vi.waitFor(() => expect(screen.queryByText("docker.io/itzg/minecraft-server:java21")).toBeNull());
    expect(screen.getByText("registry.felis.svc:5000/paper:demo")).toBeTruthy();
  });
});

describe("ImageAdmin build pipeline card", () => {
  const renderPage = () =>
    render(
      <MemoryRouter>
        <ImageAdmin />
      </MemoryRouter>,
    );
  const card = () => screen.getByRole("link", { name: new RegExp(t("admin:builds_title")) });

  it("counts builds, whatever the number of images, and leads to the pipeline", async () => {
    calls.listImages.mockResolvedValue([
      { image_ref: REF, source: "external", enabled: true },
      { image_ref: `${REF}-b`, source: "external", enabled: false },
    ]);
    calls.listBuilds.mockResolvedValue({
      builds: [{ id: "b-7", image_ref: REF, status: "succeeded", requested_by: "admin", created_at: "2026-09-01T00:00:00Z" }],
      total: 7,
    });
    renderPage();

    await vi.waitFor(() => expect(card().textContent).toBe(`7${t("admin:builds_title")}`));
    expect(card().getAttribute("href")).toBe("/admin/builds");
    expect(calls.listBuilds.mock.calls).toEqual([[{ limit: 1 }]]);
  });

  it("shows a dash when the build history can't be read", async () => {
    calls.listBuilds.mockRejectedValue({ status: 503, code: "build_unavailable", message: "builder off" });
    renderPage();
    await screen.findByText(REF);

    await vi.waitFor(() => expect(card().textContent).toBe(`—${t("admin:builds_title")}`));
  });
});
