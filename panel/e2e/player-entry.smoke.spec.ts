import { test, expect, t, expectFitsScreen } from "./fixtures";

test("entry policy keeps its form visible, saves direct routing and retains Limbo control", async ({ page, signIn }) => {
  await signIn("owner");
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/api/v1/settings/entry-policy", async (route) => { await pending; await route.continue(); });
  await page.goto("/admin/platform");
  const direct = page.getByRole("radio", { name: `${t("admin:entry_direct")} ${t("admin:entry_direct_hint")}` });
  await expect(direct).toBeVisible();
  await expect(direct).toBeDisabled();
  release();
  await expect(direct).toBeEnabled();
  const selected = page.getByRole("radio", { checked: true });
  await selected.focus();
  await page.keyboard.press("Home");
  await expect(page.getByRole("radio", { name: `${t("admin:entry_lobby")} ${t("admin:entry_lobby_hint")}` })).toBeFocused();
  await page.keyboard.press("ArrowRight");
  await expect(direct).toBeFocused();
  await expect(page.getByLabel(t("admin:entry_link"), { exact: true })).toHaveAttribute("aria-checked", "false");
  await expect(page.getByLabel(t("admin:entry_waiting"))).toHaveCount(0);
  await page.getByLabel(t("admin:entry_main"), { exact: true }).click();
  await page.getByRole("option", { name: /survival/i }).click();
  await page.getByRole("button", { name: t("admin:entry_save") }).click();
  await expect(page.getByText(t("admin:entry_saved"), { exact: true })).toBeVisible();
  await page.reload();
  await expect(direct).toHaveAttribute("aria-checked", "true");
  await expect(page.getByLabel(t("admin:entry_main"), { exact: true })).toContainText("survival");
  await page.getByLabel(t("admin:entry_offline")).click();
  await page.getByRole("option", { name: t("admin:entry_wake"), exact: true }).click();
  await expect(page.getByLabel(t("admin:entry_waiting"))).toContainText(t("admin:entry_wait_login"));
  await direct.click();
  await expect(page.getByLabel(t("admin:entry_waiting"))).toContainText(t("admin:entry_wait_login"));
  await page.getByLabel(t("admin:entry_link"), { exact: true }).click();
  await expect(page.getByLabel(t("admin:entry_waiting"))).toBeDisabled();
  await expect(page.getByLabel(t("admin:entry_waiting"))).toContainText(t("admin:entry_lobby"));
  await page.getByLabel(t("admin:entry_link"), { exact: true }).click();
  for (const viewport of [{ width: 1440, height: 1050 }, { width: 375, height: 812 }]) {
    await page.setViewportSize(viewport);
    await expectFitsScreen(page);
  }
  await page.setViewportSize({ width: 1440, height: 1050 });
  await page.getByRole("main").evaluate((el) => { el.scrollTop = 0; });
  await expect(direct).toBeInViewport();
  await page.screenshot({ path: "/tmp/felis-player-entry.png" });
});
