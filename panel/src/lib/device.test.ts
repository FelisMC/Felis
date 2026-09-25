import { describe, it, expect } from "vitest";
import i18next from "i18next";
import { deviceLabel, guessDevice } from "./device";

// Real User-Agent strings, as the browsers send them.
const UA = {
  chromeWindows:
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
  edgeWindows:
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.2739.42",
  firefoxLinux: "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0",
  safariMac:
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15",
  safariIphone:
    "Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
  chromeIphone:
    "Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/128.0.6613.98 Mobile/15E148 Safari/604.1",
  chromeAndroid:
    "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Mobile Safari/537.36",
  samsungAndroid:
    "Mozilla/5.0 (Linux; Android 14; SM-S921B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/25.0 Chrome/121.0.0.0 Mobile Safari/537.36",
  operaMac:
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Safari/537.36 OPR/113.0.0.0",
  chromebook:
    "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
};

describe("guessDevice", () => {
  it.each([
    ["chromeWindows", "Chrome", "Windows", false],
    ["edgeWindows", "Edge", "Windows", false],
    ["firefoxLinux", "Firefox", "Linux", false],
    ["safariMac", "Safari", "macOS", false],
    ["safariIphone", "Safari", "iPhone", true],
    ["chromeIphone", "Chrome", "iPhone", true],
    ["chromeAndroid", "Chrome", "Android", true],
    ["samsungAndroid", "Samsung Internet", "Android", true],
    ["operaMac", "Opera", "macOS", false],
    ["chromebook", "Chrome", "ChromeOS", false],
  ] as const)("names %s", (key, browser, os, mobile) => {
    expect(guessDevice(UA[key])).toEqual({ browser, os, mobile });
  });

  it("leaves what it cannot tell as null", () => {
    expect(guessDevice("curl/8.9.1")).toEqual({ browser: null, os: null, mobile: false });
    expect(guessDevice("")).toEqual({ browser: null, os: null, mobile: false });
  });
});

describe("deviceLabel", () => {
  const t = i18next.t.bind(i18next);

  it("names the browser and the system when both show", () => {
    expect(deviceLabel(UA.edgeWindows, t)).toBe("Edge on Windows");
    expect(deviceLabel(UA.safariIphone, t)).toBe("Safari on iPhone");
  });

  it("falls back to whichever half it can tell, then to a generic name", () => {
    expect(deviceLabel("Mozilla/5.0 (Linux x86_64) okhttp/4.12", t)).toBe("Linux");
    expect(deviceLabel("Mozilla/5.0 Firefox/130.0", t)).toBe("Firefox");
    expect(deviceLabel("curl/8.9.1", t)).toBe("Unknown device");
  });
});
