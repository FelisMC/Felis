import { test, expect, t, expectFitsScreen } from "./fixtures";

test("source disclosure does not scale on press and long pages keep bottom breathing room", async ({ page, signIn }) => {
  await signIn("owner");
  for (const viewport of [{ width: 1440, height: 600 }, { width: 375, height: 812 }]) {
    await page.setViewportSize(viewport);
    await page.goto("/admin/auth-sources");
    const disclosure = page.getByRole("button", { name: t("authSources:expand", { name: "littleskin" }) });
    const box = await disclosure.boundingBox();
    await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
    await page.mouse.down();
    await expect.poll(() => disclosure.evaluate((el) => getComputedStyle(el).transform)).toMatch(/^(none|matrix\(1, 0, 0, 1, 0, 0\))$/);
    await page.mouse.up();
    await expect(page.getByRole("button", { name: t("authSources:collapse", { name: "littleskin" }) })).toHaveAttribute("aria-expanded", "true");
    const main = page.getByRole("main");
    await main.evaluate((el) => { el.scrollTop = el.scrollHeight; });
    const save = page.getByRole("button", { name: t("authSources:save"), exact: true });
    await expect(save).toBeInViewport();
    const room = await save.evaluate((el) => el.closest("main")!.getBoundingClientRect().bottom - el.parentElement!.parentElement!.getBoundingClientRect().bottom);
    expect(room).toBeGreaterThanOrEqual(23);
    await expectFitsScreen(page);
  }
});

test("platform maintenance stays closed to an Admin who is not the Owner", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.patch("/api/v1/users/user", { data: { role: "admin" } })).ok()).toBe(true);
  await signIn("user");
  await page.goto("/admin/updates");
  await expect(page.getByText(t("common:not_authorized_title"))).toBeVisible();
  await expect(page.getByRole("heading", { name: t("admin:updates_title"), exact: true })).toHaveCount(0);
});
