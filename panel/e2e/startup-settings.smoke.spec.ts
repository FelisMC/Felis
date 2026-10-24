import { test, expect, t, expectFitsScreen } from "./fixtures";

const settingsPath = (name: string) => `/api/v1/servers/${name}/startup-settings`;
const itemName = (key: "java_flags" | "args" | "stop_commands", number: number) => t("servers:edit_startup_item", { label: t(`servers:edit_startup_${key}`), number });
const initial = {
  java_flags: ["-Dfelis.demo=true"],
  args: ["--message=hello world", "", "first\nsecond"],
  env: [{ name: "USER_MESSAGE", value: "  literal = value\nsecond line  " }],
  stop_commands: ["say Server closing", "stop"],
};

test("a server owner edits private startup settings with literal arguments and environment values on a narrow screen", async ({ page, signIn }) => {
  await signIn("user");
  expect((await page.request.patch("/api/v1/me/servers/creative", { data: initial })).ok()).toBe(true);
  expect((await page.request.post("/api/v1/servers/creative/wake")).status()).toBe(202);
  await page.setViewportSize({ width: 375, height: 812 });
  let reads = 0;
  page.on("request", (request) => { if (new URL(request.url()).pathname === settingsPath("creative")) reads++; });
  await page.goto("/servers/creative");
  await page.getByRole("button", { name: new RegExp(`^${t("servers:edit_server_title")}`) }).click();
  const dialog = page.getByRole("dialog");
  expect(reads).toBe(0);
  const advanced = dialog.getByRole("button", { name: t("servers:edit_startup_title"), exact: true });
  await expect(advanced).toHaveAttribute("aria-expanded", "false");
  await advanced.click();
  await expect(dialog.getByRole("textbox", { name: itemName("args", 1), exact: true })).toHaveValue(initial.args[0]);
  await expect(dialog.getByRole("textbox", { name: itemName("args", 2), exact: true })).toHaveValue("");
  await expect(dialog.getByRole("textbox", { name: itemName("args", 3), exact: true })).toHaveValue("first\nsecond");
  await expect(dialog.getByRole("textbox", { name: t("servers:edit_startup_env_value", { number: 1 }), exact: true })).toHaveValue(initial.env[0].value);
  const save = dialog.getByRole("button", { name: t("servers:edit_server_submit"), exact: true });
  await expect(save).toBeDisabled();
  const changed = "  one argument with spaces\nand a newline  ";
  await dialog.getByRole("textbox", { name: itemName("args", 2), exact: true }).fill(changed);
  await dialog.getByRole("button", { name: t("servers:edit_startup_env_add"), exact: true }).click();
  await dialog.getByRole("textbox", { name: t("servers:edit_startup_env_name", { number: 2 }), exact: true }).fill("USER_EMPTY");
  await expect(dialog.getByText(t("servers:edit_startup_restart_hint"), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
  const patch = page.waitForRequest((request) => request.method() === "PATCH" && new URL(request.url()).pathname === "/api/v1/me/servers/creative");
  await save.click();
  expect((await patch).postDataJSON()).toEqual({ args: [initial.args[0], changed, initial.args[2]], env: [...initial.env, { name: "USER_EMPTY", value: "" }] });
  await expect(dialog).toHaveCount(0);
  const saved = await (await page.request.get(settingsPath("creative"))).json();
  expect(saved).toEqual({ ...initial, args: [initial.args[0], changed, initial.args[2]], env: [...initial.env, { name: "USER_EMPTY", value: "" }] });
  const status = await (await page.request.get("/api/v1/servers/creative/status")).json();
  expect(status.phase).toBe("Starting");
  for (const key of ["java_flags", "args", "env", "stop_commands"]) expect(status).not.toHaveProperty(key);
  expect(reads).toBe(1);
});

test("administrators retain rejected environment drafts, clear stop commands and keep startup settings private", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.patch("/api/v1/servers/lobby", { data: initial })).ok()).toBe(true);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby");
  await page.getByRole("button", { name: new RegExp(`^${t("servers:edit_server_title")}`) }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: t("servers:edit_startup_title"), exact: true }).click();
  const name = dialog.getByRole("textbox", { name: t("servers:edit_startup_env_name", { number: 1 }), exact: true });
  await name.fill("FELIS_USER_OVERRIDE");
  const save = dialog.getByRole("button", { name: t("servers:edit_server_submit"), exact: true });
  await save.click();
  await expect(dialog.getByRole("alert")).toHaveText(t("errors:bad_startup_settings"));
  await expect(name).toHaveValue("FELIS_USER_OVERRIDE");
  await expect(dialog.getByRole("textbox", { name: t("servers:edit_startup_env_value", { number: 1 }), exact: true })).toHaveValue(initial.env[0].value);
  await name.fill("USER_MESSAGE");
  const remove = dialog.getByRole("button", { name: t("servers:edit_startup_remove_item", { label: t("servers:edit_startup_stop_commands"), number: 1 }), exact: true });
  await remove.click();
  await remove.click();
  const patch = page.waitForRequest((request) => request.method() === "PATCH" && new URL(request.url()).pathname === "/api/v1/servers/lobby");
  await save.click();
  expect((await patch).postDataJSON()).toEqual({ stop_commands: [] });
  await expect(dialog).toHaveCount(0);
  expect((await (await page.request.get(settingsPath("lobby"))).json()).stop_commands).toEqual([]);
  await expectFitsScreen(page);
  await signIn("user");
  await page.goto("/servers/claim-me");
  await expect(page.getByRole("heading", { name: "Claimable Node", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: new RegExp(`^${t("servers:edit_server_title")}`) })).toHaveCount(0);
  expect((await page.request.get(settingsPath("claim-me"))).status()).toBe(403);
  expect((await page.request.patch("/api/v1/me/servers/claim-me", { data: { args: ["private"] } })).status()).toBe(403);
  const publicStatus = await (await page.request.get("/api/v1/servers/claim-me/status")).json();
  for (const key of ["java_flags", "args", "env", "stop_commands"]) expect(publicStatus).not.toHaveProperty(key);
});
