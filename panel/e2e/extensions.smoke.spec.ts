import { test, expect, t, expectFitsScreen } from "./fixtures";

test("installed extensions fit a narrow screen", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/files/mkdir?path=plugins/broken")).ok()).toBe(true);
  expect((await page.request.post("/api/v1/servers/lobby/files/mkdir?path=config/example_core")).ok()).toBe(true);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby/extensions");
  await expect(page.getByText(t("servers:extensions_restart_hint"), { exact: true })).toBeVisible();
  await expect(page.getByRole("article").first()).toBeVisible();
  await expectFitsScreen(page);
});
