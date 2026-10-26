import { test, expect, t, expectFitsScreen } from "./fixtures";

test("Owner downloads a support bundle from the maintenance page", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/updates");
  await page.getByRole("tab", { name: t("admin:maintenance_tab_diagnostics") }).click();
  await page.getByRole("button", { name: t("admin:support_create"), exact: true }).click();
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: t("admin:host_task_download"), exact: true }).click();
  expect((await download).suggestedFilename()).toMatch(/^felis-support-.*\.tar\.gz$/);

  for (const viewport of [{ width: 1440, height: 1050 }, { width: 375, height: 812 }]) {
    await page.setViewportSize(viewport);
    await expectFitsScreen(page);
  }
});

test("host access settings fit a narrow screen", async ({ page, signIn }) => {
  await signIn("owner");
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/admin/platform?tab=access");
  await expect(page.getByRole("heading", { name: t("admin:access_title"), exact: true })).toBeVisible();
  await expect(page.getByLabel(t("admin:proxy_heap"), { exact: true })).toBeEditable();
  await expect(page.getByRole("button", { name: t("admin:credentials_rotate"), exact: true }).first()).toBeVisible();
  await expectFitsScreen(page);
});

// The content area is one scroll container for every section, so another tab has to bring it
// back to the top; without that it opens wherever the last one was scrolled to.
test("another tab opens at its top", async ({ page, signIn }) => {
  await signIn("owner");
  await page.setViewportSize({ width: 1280, height: 700 });
  await page.goto("/admin/platform?tab=notices");
  await expect(page.getByRole("tab", { name: t("admin:platform_tab_notices") })).toHaveAttribute("aria-selected", "true");
  const main = page.getByRole("main");
  const scrolled = async () => main.evaluate((el) => el.scrollTop);
  await main.evaluate((el) => { el.scrollTop = el.scrollHeight; });
  expect(await scrolled()).toBeGreaterThan(200);
  await page.getByRole("tab", { name: t("admin:platform_tab_access") }).click();
  await expect(page).toHaveURL(/tab=access/);
  await expect.poll(scrolled).toBe(0);
});
