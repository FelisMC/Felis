import type { Page } from "@playwright/test";
import { test, expect, t } from "./fixtures";

// Every control a screen reader can land on must say what it does. The sweep
// reads each page's accessibility tree (what assistive tech gets) and fails on
// a button, link, field or toggle whose accessible name is empty, which is
// what an icon-only button without aria-label turns into.
const CONTROL_ROLES = ["button", "link", "combobox", "checkbox", "switch", "textbox", "menuitem", "tab", "slider"];
// A named control reads `- button "Save"`; an unnamed one has nothing quoted
// after the role, only [state] flags and maybe its value (`- combobox: Presets`).
const UNNAMED = new RegExp(`^\\s*- (${CONTROL_ROLES.join("|")})(?: \\[[^\\]]*\\])*(?::.*)?$`);

async function unnamedControls(page: Page): Promise<string[]> {
  const tree = await page.locator("body").ariaSnapshot();
  const lines = tree.split("\n");
  // Each offender is reported with the lines under it, so the failure shows
  // which icon or field it was.
  return lines.flatMap((line, i) => (UNNAMED.test(line) ? [lines.slice(i, i + 3).join("\n")] : []));
}

for (const [account, path] of [
  ["linked", "/"],
  ["linked", "/servers"],
  ["linked", "/servers/lobby"],
  ["linked", "/servers/lobby/players"],
  ["linked", "/servers/lobby/backups"],
  ["linked", "/servers/lobby/files"],
  ["linked", "/servers/lobby/luckperms"],
  ["linked", "/submissions"],
  ["linked", "/account"],
  ["owner", "/"],
  ["owner", "/servers"],
  ["owner", "/admin/images"],
  ["owner", "/admin/builds"],
  ["owner", "/admin/submissions"],
  ["owner", "/admin/updates"],
  ["owner", "/admin/users"],
  ["owner", "/account"],
] as const) {
  test(`every control on ${path} has a name for ${account}`, async ({ page, signIn }) => {
    await signIn(account);
    await page.goto(path);
    await page.waitForLoadState("networkidle");

    expect(await unnamedControls(page)).toEqual([]);
  });
}

// Dialogs only exist once opened, so each is opened and swept on its own.
for (const [account, path, opener] of [
  ["owner", "/servers", "servers:create_server_btn"],
  ["owner", "/servers/lobby", "servers:edit_server_title"],
  ["owner", "/admin/users", "admin:users_create_btn"],
  ["owner", "/admin/images", "admin:add_image_btn"],
  ["owner", "/admin/builds", "admin:trigger_build_title"],
  ["linked", "/submissions", "submissions:submit_card_title"],
] as const) {
  test(`every control in the ${opener} dialog has a name`, async ({ page, signIn }) => {
    await signIn(account);
    await page.goto(path);
    await page.getByRole("button", { name: t(opener) }).first().click();
    await expect(page.getByRole("dialog")).toBeVisible();
    await page.waitForLoadState("networkidle");

    expect(await unnamedControls(page)).toEqual([]);
  });
}

test("every control on a user's detail page has a name", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/users");
  await page.getByRole("link", { name: "linked", exact: true }).click();
  await expect(page.getByRole("heading", { name: "linked", exact: true })).toBeVisible();
  await page.waitForLoadState("networkidle");

  expect(await unnamedControls(page)).toEqual([]);
});

test("every control on a player's LuckPerms groups and nodes has a name", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/servers/survival/luckperms");
  await page.getByRole("textbox", { name: t("servers:luckperms_player_lookup") }).fill("Steve");
  await page.keyboard.press("Enter");
  await expect(page.getByRole("cell", { name: "essentials.fly", exact: true })).toBeVisible();

  expect(await unnamedControls(page)).toEqual([]);
});

test("every control on the sign-in page has a name", async ({ page }) => {
  await page.goto("/login");
  await page.waitForLoadState("networkidle");

  expect(await unnamedControls(page)).toEqual([]);
});

test("the submission's file picker is reachable from the keyboard", async ({ page, signIn }) => {
  await signIn("linked");
  await page.goto("/submissions");
  await page.getByRole("button", { name: t("submissions:submit_card_title") }).first().click();

  const picker = page.getByLabel(t("submissions:file_label"));
  await page.getByLabel(t("submissions:display_name_label")).focus();
  for (let i = 0; i < 10 && !(await picker.evaluate((el) => el === document.activeElement)); i++) {
    await page.keyboard.press("Tab");
  }
  await expect(picker).toBeFocused();
});
