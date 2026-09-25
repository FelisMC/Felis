// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import i18next from "i18next";
import { ImageBuildPage } from "./ImageBuildPage";
import type { Build } from "@/lib/types";

const calls = vi.hoisted(() => ({
  listBuilds: vi.fn(),
  cancelBuild: vi.fn(),
  listSubmissions: vi.fn(),
  buildImage: vi.fn(),
}));
vi.mock("@/lib/tier", () => ({
  useTier: () => ({
    loading: false,
    identity: { user_id: "owner-1", email: "owner@example.test", role: "owner" },
    isAdmin: true,
    isOwner: true,
  }),
}));
vi.mock("@/lib/config", () => ({ loadConfig: () => Promise.resolve({}) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string) => i18next.t(key);
// Started by another admin in another browser: only the server knows of it.
const RUNNING: Build = {
  id: "b-1",
  image_ref: "registry.felis.svc:5000/modpack:7",
  status: "building",
  requested_by: "admin2@example.test",
  created_at: new Date().toISOString(),
};
const MINE: Build = {
  id: "b-0",
  image_ref: "registry.felis.svc:5000/paper:3",
  status: "succeeded",
  requested_by: "owner@example.test",
  created_at: new Date(Date.now() - 3600_000).toISOString(),
  finished_at: new Date(Date.now() - 3500_000).toISOString(),
};

function page(builds: Build[], total = builds.length) {
  return { builds, total };
}

beforeEach(() => {
  for (const fn of Object.values(calls)) fn.mockReset();
  calls.listBuilds.mockResolvedValue(page([RUNNING, MINE]));
  vi.spyOn(window, "confirm").mockImplementation(() => {
    throw new Error("window.confirm used");
  });
  vi.spyOn(window, "alert").mockImplementation(() => {
    throw new Error("window.alert used");
  });
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  return i18next.changeLanguage("en-US");
});

function renderPage() {
  return render(
    <MemoryRouter>
      <ImageBuildPage />
    </MemoryRouter>,
  );
}

async function openCancel() {
  renderPage();
  await userEvent.click(await screen.findByRole("button", { name: t("admin:cancel_build_btn") }));
  return screen.getByRole("dialog", { name: t("admin:cancel_build_title") });
}

describe("ImageBuildPage list", () => {
  it("lists the server's builds, including one another admin started, and marks the caller's own", async () => {
    renderPage();
    const running = (await screen.findByText(RUNNING.image_ref)).closest("li")!;
    expect(calls.listBuilds).toHaveBeenCalledWith({ query: undefined, limit: 10, offset: 0 });
    expect(within(running).getByRole("button", { name: t("admin:cancel_build_btn") })).toBeTruthy();
    expect(within(running).queryByText("You")).toBeNull();

    const mine = screen.getByText(MINE.image_ref).closest("li")!;
    expect(within(mine).getByText("You")).toBeTruthy();
  });

  it("asks the server for the page picked", async () => {
    calls.listBuilds.mockResolvedValue(page([MINE], 25));
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: t("common:pagination_next") }));
    await vi.waitFor(() => expect(calls.listBuilds).toHaveBeenLastCalledWith({ query: undefined, limit: 10, offset: 10 }));
  });

  it("searches on the server, sending a status typed as the badge shows it by its code, from page one", async () => {
    await i18next.changeLanguage("zh-CN");
    calls.listBuilds.mockResolvedValue(page([MINE], 25));
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: t("common:pagination_next") }));
    await vi.waitFor(() => expect(calls.listBuilds).toHaveBeenLastCalledWith(expect.objectContaining({ offset: 10 })));

    await userEvent.type(screen.getByRole("textbox"), "失败");
    await vi.waitFor(() =>
      expect(calls.listBuilds).toHaveBeenLastCalledWith({ query: "failed", limit: 10, offset: 0 }),
    );
  });

  it("follows the page while a build on it runs, and stops once none does", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderPage();
    await screen.findByText(RUNNING.image_ref);
    const before = calls.listBuilds.mock.calls.length;

    calls.listBuilds.mockResolvedValue(page([{ ...RUNNING, status: "succeeded" }, MINE]));
    await vi.advanceTimersByTimeAsync(4000);
    expect(calls.listBuilds.mock.calls.length).toBe(before + 1);
    await vi.waitFor(() => expect(screen.queryByRole("button", { name: t("admin:cancel_build_btn") })).toBeNull());

    await vi.advanceTimersByTimeAsync(12000);
    expect(calls.listBuilds.mock.calls.length).toBe(before + 1);
  });

  it("goes back to the top of the full list after starting a build, and opens its log", async () => {
    // The log console attaches over SSE; jsdom has no EventSource.
    vi.stubGlobal(
      "EventSource",
      class {
        readyState = 0;
        onopen = null;
        onmessage = null;
        onerror = null;
        addEventListener() {}
        close() {}
      },
    );
    calls.listSubmissions.mockResolvedValue([]);
    calls.listBuilds.mockResolvedValue(page([MINE], 25));
    const fresh: Build = { ...RUNNING, id: "b-new", image_ref: "registry.felis.svc:5000/fresh:1", requested_by: "owner@example.test" };
    calls.buildImage.mockResolvedValue(fresh);
    renderPage();
    await userEvent.type(await screen.findByRole("textbox"), "paper");
    await vi.waitFor(() => expect(calls.listBuilds).toHaveBeenLastCalledWith(expect.objectContaining({ query: "paper" })));
    await userEvent.click(await screen.findByRole("button", { name: t("common:pagination_next") }));
    await vi.waitFor(() =>
      expect(calls.listBuilds).toHaveBeenLastCalledWith({ query: "paper", limit: 10, offset: 10 }),
    );

    calls.listBuilds.mockResolvedValue(page([fresh, MINE], 26));
    await userEvent.click(screen.getByRole("button", { name: t("admin:trigger_build_title") }));
    const form = screen.getByRole("dialog", { name: t("admin:trigger_build_title") });
    await userEvent.type(within(form).getByLabelText(new RegExp(t("admin:image_ref_label"))), fresh.image_ref);
    await userEvent.type(within(form).getByLabelText(new RegExp(t("admin:context_ref_label"))), "http://ctx/x.tgz");
    await userEvent.type(within(form).getByLabelText(new RegExp(t("admin:dockerfile_label"))), "FROM scratch");
    await userEvent.click(within(form).getByRole("button", { name: t("admin:trigger_build_btn") }));

    await vi.waitFor(() =>
      expect(calls.listBuilds).toHaveBeenLastCalledWith({ query: undefined, limit: 10, offset: 0 }),
    );
    expect((screen.getByRole("textbox") as HTMLInputElement).value).toBe("");
    const row = (await screen.findByText(fresh.image_ref)).closest("li")!;
    expect(within(row).getByRole("button", { name: t("admin:view_logs_btn") }).getAttribute("aria-expanded")).toBe("true");

    // Already at the top: nothing to reset, and the list still has to be read again.
    const again: Build = { ...fresh, id: "b-again", image_ref: "registry.felis.svc:5000/again:1" };
    calls.buildImage.mockResolvedValue(again);
    calls.listBuilds.mockResolvedValue(page([again, fresh, MINE], 27));
    await userEvent.click(screen.getByRole("button", { name: t("admin:trigger_build_title") }));
    const form2 = screen.getByRole("dialog", { name: t("admin:trigger_build_title") });
    await userEvent.type(within(form2).getByLabelText(new RegExp(t("admin:image_ref_label"))), again.image_ref);
    await userEvent.type(within(form2).getByLabelText(new RegExp(t("admin:context_ref_label"))), "http://ctx/y.tgz");
    await userEvent.type(within(form2).getByLabelText(new RegExp(t("admin:dockerfile_label"))), "FROM scratch");
    await userEvent.click(within(form2).getByRole("button", { name: t("admin:trigger_build_btn") }));
    expect(await screen.findByText(again.image_ref)).toBeTruthy();
  });

  it("says a first load failed and retries it", async () => {
    calls.listBuilds.mockRejectedValueOnce({ status: 409, code: "test", message: "database is away" });
    renderPage();
    expect((await screen.findByRole("alert")).textContent).toBe("database is away");

    await userEvent.click(screen.getByRole("button", { name: t("common:try_again") }));
    expect(await screen.findByText(RUNNING.image_ref)).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("ImageBuildPage cancel", () => {
  it("asks in a dialog that names the image, then stops the build", async () => {
    calls.cancelBuild.mockResolvedValue(undefined);
    const dialog = await openCancel();
    expect(within(dialog).getByText(RUNNING.image_ref)).toBeTruthy();
    expect(calls.cancelBuild).not.toHaveBeenCalled();

    calls.listBuilds.mockResolvedValue(page([{ ...RUNNING, status: "cancelled" }, MINE]));
    await userEvent.click(within(dialog).getByRole("button", { name: t("admin:cancel_build_confirm") }));
    expect(calls.cancelBuild).toHaveBeenCalledWith(RUNNING.id);
    await vi.waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await vi.waitFor(() =>
      expect(screen.queryByRole("button", { name: t("admin:cancel_build_btn") })).toBeNull(),
    );
  });

  it("keeps the rows and says so when the list cannot be refreshed", async () => {
    calls.cancelBuild.mockResolvedValue(undefined);
    const dialog = await openCancel();
    calls.listBuilds.mockRejectedValue({ status: 409, code: "test", message: "database is away" });
    await userEvent.click(within(dialog).getByRole("button", { name: t("admin:cancel_build_confirm") }));

    expect((await screen.findByRole("alert")).textContent).toBe("Couldn't refresh the build list: database is away");
    expect(screen.getByText(RUNNING.image_ref)).toBeTruthy();
    expect(screen.getByText(MINE.image_ref)).toBeTruthy();
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
