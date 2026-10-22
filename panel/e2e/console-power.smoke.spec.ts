import { test, expect, t, expectFitsScreen } from "./fixtures";

test("console restarts and force stops an owned server", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/servers/survival");
  await page.getByRole("button", { name: t("servers:restart"), exact: true }).click();
  await expect(page.getByText(t("servers:restart_confirm_players", { count: 12 }), { exact: true })).toBeVisible();
  const restarted = page.waitForResponse((response) => response.url().endsWith("/servers/survival/restart") && response.request().method() === "POST");
  await page.getByRole("button", { name: t("servers:restart"), exact: true }).click();
  expect((await restarted).status()).toBe(202);
  await page.getByRole("button", { name: t("servers:more_actions"), exact: true }).click();
  await page.getByRole("button", { name: t("servers:kill"), exact: true }).click();
  await expect(page.getByText(t("servers:kill_warning"), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
  await page.getByRole("button", { name: t("servers:kill"), exact: true }).click();
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(page.getByRole("button", { name: t("servers:wake"), exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: t("servers:restart"), exact: true })).toBeHidden();
});

test("force stop is available to an ordinary server owner", async ({ page, signIn }) => {
  await signIn("user");
  expect((await page.request.post("/api/v1/servers/creative/wake")).status()).toBe(202);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/creative");
  await page.getByRole("button", { name: t("servers:more_actions"), exact: true }).click();
  await expect(page.getByRole("button", { name: t("servers:emergency_stop"), exact: true })).toBeHidden();
  await page.getByRole("button", { name: t("servers:kill"), exact: true }).click();
  await expect(page.getByText(t("servers:kill_warning"), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
  await page.getByRole("button", { name: t("servers:kill"), exact: true }).click();
  await expect(page.getByRole("button", { name: t("servers:wake"), exact: true })).toBeVisible();
});

test("an unrelated player cannot force stop a server", async ({ page, signIn }) => {
  await signIn("user");
  await page.goto("/servers/survival");
  await expect(page.getByRole("button", { name: t("servers:restart"), exact: true })).toBeHidden();
  await expect(page.getByRole("button", { name: t("servers:more_actions"), exact: true })).toBeHidden();
  expect((await page.request.post("/api/v1/servers/survival/kill")).status()).toBe(403);
});
