import { test, expect, t, expectFitsScreen } from "./fixtures";

test("server owners can submit during startup and the console distinguishes stdin from RCON replies", async ({ page, signIn }) => {
  await signIn("user");
  expect((await page.request.post("/api/v1/servers/creative/wake")).status()).toBe(202);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/creative");
  const input = page.getByRole("textbox", { name: t("servers:command_label"), exact: true });
  await input.fill("/list");
  const submitted = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/servers/creative/command"));
  await page.getByRole("button", { name: t("servers:command_send"), exact: true }).click();
  expect(await (await submitted).json()).toEqual({ name: "creative", output: "", transport: "stdin" });
  await expect(page.getByText(t("servers:command_sent_stdin"), { exact: true })).toBeVisible();
  await expect(input).toHaveValue("");
  await expect(page.getByRole("region", { name: t("servers:log_history_title"), exact: true })).toHaveCount(0);
  await expectFitsScreen(page);

  await signIn("owner");
  await page.goto("/servers/survival");
  await input.fill("list");
  const replied = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/servers/survival/command"));
  await input.press("Enter");
  const rconOutput = "There are 2 of a max 20 players online: mock_player, test_player";
  expect(await (await replied).json()).toEqual({ name: "survival", output: rconOutput, transport: "rcon" });
  const rconReply = page.locator("pre").filter({ hasText: rconOutput });
  await expect(rconReply).toBeVisible();
  await expect(rconReply).toHaveText("> list\n" + rconOutput);
  await expect(page.getByText(t("servers:command_sent_stdin"), { exact: true })).toHaveCount(0);

  await page.route("**/api/v1/servers/survival/command", (route) => route.fulfill({ json: { name: "survival", output: "", transport: "stdin" } }));
  await input.fill("say fallback");
  await page.getByRole("button", { name: t("servers:command_send"), exact: true }).click();
  await expect(page.getByText(t("servers:command_sent_stdin"), { exact: true })).toBeVisible();
  await expect(rconReply).toHaveCount(0);
  await expect(input).toHaveValue("");
  await expectFitsScreen(page);
});

test("uncertain commands remain visible without automatic resubmission and stopped or unrelated servers reject input", async ({ page, signIn }) => {
  await signIn("owner");
  let requests = 0;
  let release!: () => void;
  await page.route("**/api/v1/servers/survival/command", async (route) => {
    requests++;
    await new Promise<void>((done) => { release = done; });
    await route.fulfill({ status: 502, json: { error: { code: "command_outcome_unknown", message: "the command may have executed" } } });
  });
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/survival");
  const input = page.getByRole("textbox", { name: t("servers:command_label"), exact: true });
  await input.fill("say once");
  const send = page.getByRole("button", { name: t("servers:command_send"), exact: true });
  await send.click();
  await expect(input).toBeDisabled();
  await expect(input).toHaveValue("say once");
  await expect(send).toBeDisabled();
  await page.keyboard.press("Enter");
  await expect.poll(() => requests).toBe(1);
  release();
  await expect(page.getByRole("alert").getByText(t("errors:command_outcome_unknown"), { exact: true })).toBeVisible();
  await expect(input).toHaveValue("say once");
  await expect(input).toBeEnabled();
  await expect(page.getByText(t("servers:command_sent_stdin"), { exact: true })).toHaveCount(0);
  await expectFitsScreen(page);
  expect(requests).toBe(1);

  expect((await page.request.post("/api/v1/servers/survival/stop")).status()).toBe(202);
  await page.reload();
  await expect(page.getByRole("region", { name: t("servers:log_history_title"), exact: true })).toBeVisible();
  await expect(input).toHaveCount(0);
  expect(requests).toBe(1);
  expect((await page.request.post("/api/v1/servers/creative/command", { data: { command: "list" } })).status()).toBe(409);
  await signIn("user");
  await page.goto("/servers/claim-me");
  await expect(page.getByText(t("servers:server_asleep_title"), { exact: true })).toBeVisible();
  await expect(input).toHaveCount(0);
  expect((await page.request.post("/api/v1/servers/claim-me/command", { data: { command: "list" } })).status()).toBe(403);
});
