import type { TFunction } from "i18next";

/** A signed-in device as its owner would name it, read from the User-Agent the
 *  browser sent at sign-in. A part the string does not reveal stays null so the
 *  caller can fall back to a generic label. */
export interface DeviceGuess {
  browser: string | null;
  os: string | null;
  mobile: boolean;
}

// First match wins. Edge, Opera and Samsung Internet also claim Chrome, and
// Chrome claims Safari, so the specific names come first.
const BROWSERS: [RegExp, string][] = [
  [/\bEdg(?:e|A|iOS)?\//, "Edge"],
  [/\b(?:OPR|Opera)\//, "Opera"],
  [/\bSamsungBrowser\//, "Samsung Internet"],
  [/\b(?:Firefox|FxiOS)\//, "Firefox"],
  [/\b(?:Chrome|CriOS)\//, "Chrome"],
  [/\bVersion\/[\d.]+.*\bSafari\//, "Safari"],
];

// iPhones say "like Mac OS X" and Android says Linux, so they come first.
const SYSTEMS: [RegExp, string][] = [
  [/\b(?:iPhone|iPod)\b/, "iPhone"],
  [/\biPad\b/, "iPad"],
  [/\bAndroid\b/, "Android"],
  [/\bCrOS\b/, "ChromeOS"],
  [/\bWindows\b/, "Windows"],
  [/\bMacintosh\b|\bMac OS X\b/, "macOS"],
  [/\bLinux\b/, "Linux"],
];

function first(table: [RegExp, string][], ua: string): string | null {
  return table.find(([re]) => re.test(ua))?.[1] ?? null;
}

export function guessDevice(userAgent: string): DeviceGuess {
  return {
    browser: first(BROWSERS, userAgent),
    os: first(SYSTEMS, userAgent),
    mobile: /\b(?:Mobile|iPhone|iPod|Android)\b/.test(userAgent),
  };
}

/** deviceLabel names a session's device for a list row: "Chrome on Windows",
 *  or whichever half the User-Agent reveals, or "Unknown device". */
export function deviceLabel(userAgent: string, t: TFunction): string {
  const { browser, os } = guessDevice(userAgent);
  if (browser && os) return t("account:session_device", { browser, os });
  return browser ?? os ?? t("account:session_device_unknown");
}
