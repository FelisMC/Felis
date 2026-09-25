import { test, expect, t, expectFitsScreen } from "./fixtures";

test("the menu drawer reaches the other sections", async ({ page, signIn }) => {
  await signIn("linked");
  await page.goto("/");

  await page.getByRole("button", { name: t("common:open_menu") }).click();
  const drawer = page.getByRole("dialog");
  await expect(drawer).toBeVisible();
  await drawer.getByRole("link", { name: t("navigation:account") }).click();

  await expect(page).toHaveURL(/\/account$/);
  await expect(drawer).toBeHidden();
});

for (const [account, path] of [
  ["linked", "/"],
  ["linked", "/servers"],
  ["linked", "/servers/lobby"],
  ["linked", "/account"],
  ["owner", "/servers"],
  ["owner", "/admin/users"],
] as const) {
  test(`${path} fits a 375px screen for ${account}`, async ({ page, signIn }) => {
    await signIn(account);
    await page.goto(path);
    await page.waitForLoadState("networkidle");

    await expectFitsScreen(page);
  });
}
