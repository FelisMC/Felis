import { readFileSync } from "node:fs";
import { test as base, expect, type Page } from "@playwright/test";

// t reads the en-US strings the panel renders, so a copy change does not
// break the smoke and a missing key does ("ns:key", optional {{count}}).
const cache = new Map<string, Record<string, string>>();
export function t(key: string, vars: Record<string, string | number> = {}): string {
  const [ns, k] = key.split(":");
  if (!cache.has(ns)) {
    const url = new URL(`../src/i18n/resources/en-US/${ns}.json`, import.meta.url);
    cache.set(ns, JSON.parse(readFileSync(url, "utf8")));
  }
  const table = cache.get(ns)!;
  const plural = typeof vars.count === "number" ? `${k}_${vars.count === 1 ? "one" : "other"}` : k;
  const text = table[plural] ?? table[k];
  if (text === undefined) throw new Error(`missing en-US string ${key}`);
  return text.replace(/\{\{(\w+)\}\}/g, (_, v: string) => String(vars[v]));
}

type Account = "owner" | "user" | "linked";

export const test = base.extend<{ signIn: (account: Account) => Promise<void> }>({
  page: async ({ page, request }, use) => {
    const res = await request.post("/api/v1/__mock/reset");
    expect(res.ok()).toBe(true);
    await use(page);
  },
  // signIn sets the mock session cookie directly; the sign-in form itself is
  // covered by its own test.
  signIn: async ({ context, baseURL }, use) => {
    await use(async (account) => {
      await context.addCookies([{ name: "felis_mock_session", value: account, url: baseURL! }]);
    });
  },
});

export { expect };

/** expectFitsScreen fails when anything scrolls sideways or a visible element
 *  pokes past the viewport edge (an overflow-hidden parent would just cut it
 *  off). The shell scrolls in an overflow-y-auto pane, whose computed
 *  overflow-x is auto as well, so a computed style cannot tell a deliberate
 *  horizontal scroller from the page pane: only a container that asks for one
 *  by class (overflow-x-auto, overflow-x-scroll, overflow-auto, e.g. a wide
 *  table) may be wider than the screen. */
export async function expectFitsScreen(page: Page) {
  const report = await page.evaluate(() => {
    const vw = document.documentElement.clientWidth;
    const deliberate = (el: Element) =>
      ["overflow-x-auto", "overflow-x-scroll", "overflow-auto"].some((c) => el.classList.contains(c));
    const inDeliberate = (el: Element) => {
      for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
        if (deliberate(p)) return true;
      }
      return false;
    };
    const name = (el: Element) => {
      const cls = typeof el.className === "string" ? el.className.split(/\s+/).slice(0, 3).join(".") : "";
      return `${el.tagName.toLowerCase()}${cls ? "." + cls : ""}`;
    };
    const scrollers: string[] = [];
    const offenders: string[] = [];
    for (const el of [document.documentElement, ...Array.from(document.body.querySelectorAll("*"))]) {
      const style = getComputedStyle(el);
      if (style.visibility === "hidden" || deliberate(el) || inDeliberate(el)) continue;
      if (el.scrollWidth > el.clientWidth + 1 && ["auto", "scroll"].includes(style.overflowX)) {
        scrollers.push(`${name(el)} scrolls ${el.scrollWidth - el.clientWidth}px sideways`);
      }
      const r = el.getBoundingClientRect();
      if (r.width === 0 || r.height === 0) continue;
      if (r.left < -1 || r.right > vw + 1) offenders.push(`${name(el)} [${Math.round(r.left)}..${Math.round(r.right)}]`);
    }
    return { vw, scrollers: scrollers.slice(0, 5), offenders: offenders.slice(0, 5) };
  });
  expect(report.scrollers, "containers that scroll sideways").toEqual([]);
  expect(report.offenders, `elements past the ${report.vw}px viewport`).toEqual([]);
}
