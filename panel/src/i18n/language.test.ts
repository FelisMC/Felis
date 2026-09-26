// @vitest-environment jsdom
import { describe, it, expect, vi, afterEach } from "vitest";
import i18next from "i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { i18nOptions } from "./index";
import { panelLanguage } from "./language";

// boot starts a fresh instance with the panel's own options, as a first visit
// with these browser languages (and whatever is in storage) would.
async function boot(languages: string[]) {
  vi.spyOn(window.navigator, "languages", "get").mockReturnValue(languages);
  vi.spyOn(window.navigator, "language", "get").mockReturnValue(languages[0] ?? "");
  const instance = i18next.createInstance().use(LanguageDetector);
  await instance.init(i18nOptions);
  return instance;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("the panel language on a first visit", () => {
  it.each([
    [["zh-CN"]],
    [["zh-TW"]],
    [["zh-HK", "en"]],
    [["zh"]],
    [["zh-Hant-TW"]],
    // English listed after Chinese must not win.
    [["zh-TW", "en-US", "en"]],
    [["zh", "en-US"]],
  ])("reads Chinese for a browser asking %j", async (languages) => {
    const i18n = await boot(languages);

    expect(i18n.language).toBe("zh-CN");
    expect(i18n.t("common:loading")).toBe(i18n.getFixedT("zh-CN")("common:loading"));
    expect(localStorage.getItem("felis-lang")).toBe("zh-CN");
  });

  it.each([[["en-GB"]], [["en-US", "zh-CN"]], [["fr-FR", "de"]], [[]]])(
    "reads English for a browser asking %j",
    async (languages) => {
      const i18n = await boot(languages);

      expect(i18n.language).toBe("en-US");
    },
  );

  it("keeps a language chosen here over the browser's", async () => {
    localStorage.setItem("felis-lang", "en-US");
    const i18n = await boot(["zh-CN"]);

    expect(i18n.language).toBe("en-US");
  });

  it("maps a regional code a past visit stored onto the shipped text", async () => {
    localStorage.setItem("felis-lang", "zh-TW");
    const i18n = await boot(["en-US"]);

    expect(i18n.language).toBe("zh-CN");
  });
});

describe("panelLanguage", () => {
  it("maps any Chinese or English code onto a shipped translation", () => {
    expect(panelLanguage("zh_TW")).toBe("zh-CN");
    expect(panelLanguage("ZH-hk")).toBe("zh-CN");
    expect(panelLanguage("en")).toBe("en-US");
    expect(panelLanguage("ja-JP")).toBe("ja-JP");
  });
});
