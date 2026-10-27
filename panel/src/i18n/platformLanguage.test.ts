// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import i18next from "i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { i18nOptions } from "./index";
import { chooseLanguage } from "./language";
import { applyPlatformLanguage } from "./platformLanguage";

const calls = vi.hoisted(() => ({ getPanelLanguage: vi.fn() }));
vi.mock("@/lib/api", () => ({ api: calls }));

async function boot(languages: string[]) {
  vi.spyOn(window.navigator, "languages", "get").mockReturnValue(languages);
  vi.spyOn(window.navigator, "language", "get").mockReturnValue(languages[0] ?? "");
  const instance = i18next.createInstance().use(LanguageDetector);
  await instance.init(i18nOptions);
  return instance;
}

beforeEach(() => {
  localStorage.clear();
  calls.getPanelLanguage.mockReset();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("the language at startup", () => {
  it("keeps the language the user chose over the platform default", async () => {
    localStorage.setItem("felis-lang", "en-US");
    calls.getPanelLanguage.mockResolvedValue({ language: "zh-CN" });
    const instance = await boot(["fr-FR"]);

    await applyPlatformLanguage(instance);

    expect(instance.language).toBe("en-US");
  });

  it("uses the platform default over the browser language without recording it as a choice", async () => {
    calls.getPanelLanguage.mockResolvedValue({ language: "zh-CN" });
    const instance = await boot(["en-US"]);

    await applyPlatformLanguage(instance);

    expect(instance.language).toBe("zh-CN");
    expect(localStorage.getItem("felis-lang")).toBeNull();
  });

  it("keeps the browser language when the platform default follows the browser", async () => {
    calls.getPanelLanguage.mockResolvedValue({ language: "" });
    const instance = await boot(["zh-TW"]);

    await applyPlatformLanguage(instance);

    expect(instance.language).toBe("zh-CN");
  });

  it("keeps the browser language when the platform default cannot be read", async () => {
    calls.getPanelLanguage.mockRejectedValue({ status: 0, code: "network_error", message: "offline" });
    const instance = await boot(["zh-TW"]);

    await applyPlatformLanguage(instance);

    expect(instance.language).toBe("zh-CN");
  });

  it("keeps a language the user chooses while the platform default is loading", async () => {
    let resolve!: (value: { language: string }) => void;
    calls.getPanelLanguage.mockReturnValue(new Promise((r) => { resolve = r; }));
    const instance = await boot(["en-US"]);
    const applied = applyPlatformLanguage(instance);

    await chooseLanguage(instance, "en-US");
    resolve({ language: "zh-CN" });
    await applied;

    expect(instance.language).toBe("en-US");
  });
});

describe("choosing a language", () => {
  it("switches the interface and records the choice for later visits", async () => {
    const instance = await boot(["en-US"]);

    await chooseLanguage(instance, "zh-CN");

    expect(instance.language).toBe("zh-CN");
    expect(localStorage.getItem("felis-lang")).toBe("zh-CN");
  });
});
