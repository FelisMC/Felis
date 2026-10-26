import { test, expect, t, expectFitsScreen } from "./fixtures";

test("the force stop confirmation fits a narrow screen", async ({ page, signIn }) => {
  await signIn("user");
  expect((await page.request.post("/api/v1/servers/creative/wake")).status()).toBe(202);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/creative");
  await page.getByRole("button", { name: t("servers:more_actions"), exact: true }).click();
  await page.getByRole("button", { name: t("servers:kill"), exact: true }).click();
  await expect(page.getByText(t("servers:kill_warning"), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
});
