// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { ImageBuildPage } from "./ImageBuildPage";
import type { Build } from "@/lib/types";

const calls = vi.hoisted(() => ({ getBuild: vi.fn(), cancelBuild: vi.fn(), listSubmissions: vi.fn() }));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({ loading: false, identity: { user_id: "owner-1", role: "owner" }, isAdmin: true, isOwner: true }),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string) => i18next.t(key);
const BUILD: Build = {
  id: "b-1",
  image_ref: "registry.felis.svc:5000/modpack:7",
  status: "building",
  requested_by: "owner-1",
  created_at: new Date().toISOString(),
};

// Node's own (unconfigured) localStorage shadows jsdom's, so give the page one.
function memoryStorage(): Storage {
  const m = new Map<string, string>();
  return {
    get length() {
      return m.size;
    },
    key: (i) => [...m.keys()][i] ?? null,
    getItem: (k) => m.get(k) ?? null,
    setItem: (k, v) => void m.set(k, String(v)),
    removeItem: (k) => void m.delete(k),
    clear: () => m.clear(),
  };
}

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  vi.stubGlobal("localStorage", memoryStorage());
  localStorage.setItem("felis_triggered_builds", JSON.stringify([BUILD.id]));
  calls.getBuild.mockResolvedValue(BUILD);
  vi.spyOn(window, "confirm").mockImplementation(() => {
    throw new Error("window.confirm used");
  });
  vi.spyOn(window, "alert").mockImplementation(() => {
    throw new Error("window.alert used");
  });
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

async function openCancel() {
  render(
    <MemoryRouter>
      <ImageBuildPage />
    </MemoryRouter>,
  );
  await userEvent.click(await screen.findByRole("button", { name: t("admin:cancel_build_btn") }));
  return screen.getByRole("dialog", { name: t("admin:cancel_build_title") });
}

describe("ImageBuildPage cancel", () => {
  it("asks in a dialog that names the image, then stops the build", async () => {
    calls.cancelBuild.mockResolvedValue(undefined);
    const dialog = await openCancel();
    expect(within(dialog).getByText(BUILD.image_ref)).toBeTruthy();
    expect(calls.cancelBuild).not.toHaveBeenCalled();

    calls.getBuild.mockResolvedValue({ ...BUILD, status: "cancelled" });
    await userEvent.click(within(dialog).getByRole("button", { name: t("admin:cancel_build_confirm") }));
    expect(calls.cancelBuild).toHaveBeenCalledWith(BUILD.id);
    await vi.waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await vi.waitFor(() =>
      expect(screen.queryByRole("button", { name: t("admin:cancel_build_btn") })).toBeNull(),
    );
  });

  it("keeps building when the dialog is dismissed", async () => {
    const dialog = await openCancel();
    await userEvent.click(within(dialog).getByRole("button", { name: t("admin:cancel_build_keep") }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(calls.cancelBuild).not.toHaveBeenCalled();
  });

  it("shows a refused cancel in the dialog", async () => {
    calls.cancelBuild.mockRejectedValue({ status: 409, code: "test", message: "build already finished" });
    const dialog = await openCancel();
    await userEvent.click(within(dialog).getByRole("button", { name: t("admin:cancel_build_confirm") }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe("build already finished");
  });
});
