// Normalize Chinese and English regional language codes before i18next selects supported translations.
import type { i18n } from "i18next";

export const SUPPORTED_LANGUAGES = ["en-US", "zh-CN"] as const;

/** Written only when the user selects a language, so a stored value is always their own choice. */
export const LANGUAGE_STORAGE_KEY = "felis-lang";

/** panelLanguage maps a detected language code onto a shipped translation, or
 *  returns it unchanged for i18next to pass over. */
export function panelLanguage(code: string): string {
  const base = code.trim().toLowerCase().split(/[-_]/)[0];
  if (base === "zh") return "zh-CN";
  if (base === "en") return "en-US";
  return code;
}

export function chosenLanguage(): string | null {
  try {
    return localStorage.getItem(LANGUAGE_STORAGE_KEY);
  } catch {
    return null;
  }
}

export function chooseLanguage(instance: i18n, language: string): Promise<unknown> {
  try {
    localStorage.setItem(LANGUAGE_STORAGE_KEY, language);
  } catch {
    /* storage is unavailable, so the choice lasts until the page closes */
  }
  return instance.changeLanguage(language);
}
