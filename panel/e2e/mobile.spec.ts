import { test, expect, t, expectFitsScreen, expectPinnedHeading } from "./fixtures";

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
  ["owner", "/admin/auth-sources"],
  ["owner", "/servers/survival/luckperms"],
] as const) {
  test(`${path} fits a 375px screen for ${account}`, async ({ page, signIn }) => {
    await signIn(account);
    await page.goto(path);
    await page.waitForLoadState("networkidle");

    await expectFitsScreen(page);
  });
}

test("the LuckPerms player list leaves the rest of the page on the first screen", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/servers/survival/luckperms");
  const list = page.getByRole("button", { name: "mock_player", exact: true }).locator("..");
  await expect(list).toBeVisible();

  // Twelve players online: the list scrolls inside its card, so the prompt
  // under it shows without scrolling the page.
  await expect(page.getByText(t("servers:luckperms_select_player_prompt"))).toBeInViewport({ ratio: 1 });
  const { scrollHeight, clientHeight } = await list.evaluate((el) => ({
    scrollHeight: el.scrollHeight,
    clientHeight: el.clientHeight,
  }));
  expect(scrollHeight).toBeGreaterThan(clientHeight);

  // The last one is still a tap away.
  await page.getByRole("button", { name: "Herobrine", exact: true }).click();
  await expect(page.getByRole("textbox", { name: t("servers:luckperms_player_lookup") })).toHaveValue("Herobrine");
});

test("login customization fits a phone and retains a saved title", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/lobby?space=login");
  await page.getByLabel(t("lobby:bookTitle")).fill("Our Network");
  await expectFitsScreen(page);
  await page.getByRole("button", { name: t("lobby:save"), exact: true }).click();
  await expect(page.getByText(t("lobby:saved"), { exact: true })).toBeVisible();
  await page.reload();
  await expect(page.getByLabel(t("lobby:bookTitle"))).toHaveValue("Our Network");
});

test("authentication source drafts with long IDs fit a phone", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/auth-sources");
  await page.getByRole("button", { name: t("authSources:add"), exact: true }).click();
  await page.getByLabel(t("authSources:tag")).nth(1).fill("a".repeat(128));
  await expectFitsScreen(page);
  await expect(page.getByRole("button", { name: t("authSources:remove"), exact: true })).toBeVisible();
});

test("the mobile navigation and page heading stay visible above a scrolling form", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/lobby");
  await expect(page.getByLabel(t("lobby:bookTitle"))).toBeEnabled();
  await expectPinnedHeading(page, t("lobby:title"), 56);
  await expect(page.getByRole("button", { name: t("common:open_menu") })).toBeInViewport({ ratio: 1 });
  await expectFitsScreen(page);
});
