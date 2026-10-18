// Normalize Chinese and English regional language codes before i18next selects supported translations.

export const SUPPORTED_LANGUAGES = ["en-US", "zh-CN"] as const;

/** panelLanguage maps a detected language code onto a shipped translation, or
 *  returns it unchanged for i18next to pass over. */
export function panelLanguage(code: string): string {
  const base = code.trim().toLowerCase().split(/[-_]/)[0];
  if (base === "zh") return "zh-CN";
  if (base === "en") return "en-US";
  return code;
}
