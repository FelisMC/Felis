import { test, expect, t, expectFitsScreen } from "./fixtures";

test("Owner manages durable authentication sources from the sidebar", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/");
  await page.getByRole("link", { name: t("navigation:auth_sources"), exact: true }).click();
  await expect(page).toHaveURL(/\/admin\/auth-sources$/);
  await expect(page.getByLabel(t("authSources:tag"))).toHaveValue("littleskin");
  await expect(page.getByLabel(t("authSources:tag"))).toHaveAttribute("readonly", "");
  await page.getByRole("button", { name: t("authSources:add"), exact: true }).click();
  await page.getByLabel(t("authSources:tag")).nth(1).fill("custom");
  await page.getByLabel(t("authSources:prefix")).nth(1).fill("CS");
  await page.getByLabel(t("authSources:url"), { exact: true }).nth(1).fill("https://custom.example/sessionserver/session/minecraft/hasJoined");
  await page.getByRole("button", { name: t("authSources:test"), exact: true }).nth(1).click();
  await expect(page.getByText(t("authSources:test_ok", { ms: 20 }), { exact: true })).toBeVisible();
  await page.getByRole("button", { name: t("authSources:move_up"), exact: true }).nth(1).click();
  await page.getByRole("checkbox", { name: t("authSources:enabled"), exact: true }).nth(1).uncheck();
  await page.getByRole("button", { name: t("authSources:save"), exact: true }).click();
  await expect(page.getByText(t("authSources:saved"), { exact: true })).toBeVisible();
  await page.reload();
  await expect(page.getByLabel(t("authSources:tag")).nth(0)).toHaveValue("custom");
  await expect(page.getByRole("checkbox", { name: t("authSources:enabled"), exact: true }).nth(1)).not.toBeChecked();
  await expect(page.getByRole("button", { name: t("authSources:remove"), exact: true })).toHaveCount(0);
  const available = await page.request.get("/api/v1/account/link/sources");
  expect((await available.json()).sources.map((source: { tag: string }) => source.tag)).toEqual(["mojang", "custom"]);
  await expectFitsScreen(page);
  await page.screenshot({ path: "/tmp/felis-auth-sources.png", fullPage: true });
});

test("source fields stay visible while initial configuration is loading", async ({ page, signIn }) => {
  await signIn("owner");
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/api/v1/settings/auth-sources", async (route) => { await pending; await route.continue(); });
  await page.goto("/admin/auth-sources");
  await expect(page.getByLabel(t("authSources:tag"))).toBeVisible();
  await expect(page.getByLabel(t("authSources:tag"))).toBeDisabled();
  await expect(page.getByText(t("authSources:loading"), { exact: true }).first()).toBeVisible();
  release();
  await expect(page.getByLabel(t("authSources:tag"))).toBeEnabled();
});

test("a player has neither an authentication source entry nor access to its API", async ({ page, signIn }) => {
  await signIn("linked");
  await page.goto("/admin/auth-sources");
  await expect(page.getByText(t("common:not_authorized_title"))).toBeVisible();
  await expect(page.getByRole("link", { name: t("navigation:auth_sources"), exact: true })).toHaveCount(0);
  const response = await page.request.get("/api/v1/settings/auth-sources");
  expect(response.status()).toBe(403);
});

for (const path of ["/servers", "/account"]) {
  test(`logout from ${path} on the operator host returns to login methods`, async ({ page, signIn }) => {
    await page.route("**/config.json", async (route) => {
      const response = await route.fetch();
      await route.fulfill({ response, json: { ...await response.json(), adminHostname: "localhost" } });
    });
    await signIn("owner");
    await page.goto(path);
    const logout = page.getByRole("button", { name: t("account:sign_out"), exact: true });
    await (path === "/account" ? logout.last() : logout.first()).click();
    await expect(page).toHaveURL(/\/login\?/);
    await expect(page.getByRole("button", { name: t("auth:passkey_btn"), exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: t("auth:tab_op_btn"), exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: t("auth:op_start_btn"), exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: t("auth:tab_op_btn"), exact: true }).click();
    await expect(page.getByRole("button", { name: t("auth:op_start_btn"), exact: true })).toBeVisible();
  });
}

test("Chinese authentication settings render in both themes", async ({ page, signIn }) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.addInitScript(() => localStorage.setItem("felis-lang", "zh-CN"));
  await signIn("owner");
  await page.emulateMedia({ colorScheme: "dark" });
  await page.goto("/admin/auth-sources");
  await expect(page.getByRole("heading", { name: "认证源", exact: true })).toBeVisible();
  await expect(page.getByLabel("永久标识")).toHaveValue("littleskin");
  await expectFitsScreen(page);
  await page.screenshot({ path: "/tmp/felis-auth-sources-zh-dark.png", fullPage: true });
  await page.getByRole("button", { name: "切换主题", exact: true }).click();
  await expectFitsScreen(page);
  await page.screenshot({ path: "/tmp/felis-auth-sources-zh-light.png", fullPage: true });
});
