// The panel ships two translations; everything a browser reports is mapped onto
// one of them before i18next picks. Any Chinese (zh, zh-TW, zh-HK, zh-Hant, a
// stale "zh_TW" in storage) reads the zh-CN text and any English the en-US
// text. Mapping at detection matters: i18next first looks for an exact
// supported code anywhere in the browser's list, so ["zh-TW", "en-US"] would
// land on English if zh-TW reached it unmapped.

export const SUPPORTED_LANGUAGES = ["en-US", "zh-CN"] as const;

/** panelLanguage maps a detected language code onto a shipped translation, or
 *  returns it unchanged for i18next to pass over. */
export function panelLanguage(code: string): string {
  const base = code.trim().toLowerCase().split(/[-_]/)[0];
  if (base === "zh") return "zh-CN";
  if (base === "en") return "en-US";
  return code;
}
