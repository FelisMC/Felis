import { test, expect, t, expectFitsScreen } from "./fixtures";

const itemName = (key: "java_flags" | "args" | "stop_commands", number: number) => t("servers:edit_startup_item", { label: t(`servers:edit_startup_${key}`), number });
const initial = {
  java_flags: ["-Dfelis.demo=true"],
  args: ["--message=hello world", "", "first\nsecond"],
  env: [{ name: "USER_MESSAGE", value: "  literal = value\nsecond line  " }],
  stop_commands: ["say Server closing", "stop"],
};

test("startup settings fit a narrow screen", async ({ page, signIn }) => {
  await signIn("user");
  expect((await page.request.patch("/api/v1/me/servers/creative", { data: initial })).ok()).toBe(true);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/creative/settings");
  await page.getByRole("button", { name: new RegExp(`^${t("servers:edit_server_title")}`) }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: t("servers:edit_startup_title"), exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: itemName("args", 1), exact: true })).toHaveValue(initial.args[0]);
  await dialog.getByRole("button", { name: t("servers:edit_startup_env_add"), exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: t("servers:edit_startup_env_name", { number: 2 }), exact: true })).toBeVisible();
  await expectFitsScreen(page);
});
