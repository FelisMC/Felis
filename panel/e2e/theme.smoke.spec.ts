import { test, expect, t, expectFitsScreen } from "./fixtures";

test("theme choices persist and follow the system only when selected", async ({ page, signIn }) => {
  await signIn("user");
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/servers");
  const root = page.locator("html");
  const theme = page.getByRole("combobox", { name: t("common:toggle_theme"), exact: true });
  await theme.focus();
  await page.keyboard.press("Enter");
  await page.getByRole("option", { name: t("common:theme_dark"), exact: true }).click();
  await expect(root).toHaveClass(/dark/);
  await page.reload();
  await expect(root).toHaveClass(/dark/);
  await theme.click();
  await page.getByRole("option", { name: t("common:theme_system"), exact: true }).click();
  await expect(root).not.toHaveClass(/dark/);
  await page.emulateMedia({ colorScheme: "dark" });
  await expect(root).toHaveClass(/dark/);
  await page.setViewportSize({ width: 375, height: 812 });
  await theme.click();
  await page.getByRole("option", { name: t("common:theme_light"), exact: true }).click();
  await expect(root).not.toHaveClass(/dark/);
  await page.reload();
  await expect(root).not.toHaveClass(/dark/);
  await expectFitsScreen(page);
});
