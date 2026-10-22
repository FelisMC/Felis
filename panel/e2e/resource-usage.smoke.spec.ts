import { test, expect, t, expectFitsScreen } from "./fixtures";

test("owners can view current resources and the last hour of charts", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/servers/survival");
  await expect(page.getByLabel(t("servers:usage_title"), { exact: true })).toBeVisible();
  await expect(page.getByRole("img", { name: t("servers:usage_cpu_curve"), exact: true })).toBeVisible();
  await expect(page.getByRole("img", { name: t("servers:usage_memory_curve"), exact: true })).toBeVisible();
  await expect(page.getByText(t("servers:usage_cpu_basis"), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
});
