import { test, expect, t, expectFitsScreen } from "./fixtures";

test("platform policy is an Owner entry and persists after reload", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/platform");
  await expect(page.getByRole("heading", { name: t("admin:platform_title"), exact: true })).toBeVisible();
  const limit = page.getByLabel(t("admin:platform_limit"));
  await expect(limit).toBeEnabled();
  await limit.fill("3");
  await page.getByLabel(t("admin:platform_cooldown")).fill("45");
  await page.getByRole("button", { name: t("admin:platform_save") }).click();
  await expect(page.getByText(t("admin:platform_saved"), { exact: true })).toBeVisible();
  await page.reload();
  await expect(limit).toHaveValue("3");
  await expect(page.getByLabel(t("admin:platform_cooldown"))).toHaveValue("45");
  await expectFitsScreen(page);
  await signIn("linked");
  await page.goto("/admin/platform");
  await expect(page.getByRole("heading", { name: t("admin:platform_title"), exact: true })).toBeHidden();
  expect((await page.request.get("/api/v1/settings/wake-policy")).status()).toBe(403);
});

test("source disclosure does not scale on press and long pages keep bottom breathing room", async ({ page, signIn }) => {
  await signIn("owner");
  for (const viewport of [{ width: 1440, height: 600 }, { width: 375, height: 812 }]) {
    await page.setViewportSize(viewport);
    await page.goto("/admin/auth-sources");
    const disclosure = page.getByRole("button", { name: t("authSources:expand", { name: "littleskin" }) });
    const box = await disclosure.boundingBox();
    await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
    await page.mouse.down();
    await expect.poll(() => disclosure.evaluate((el) => getComputedStyle(el).transform)).toMatch(/^(none|matrix\(1, 0, 0, 1, 0, 0\))$/);
    await page.mouse.up();
    await expect(page.getByRole("button", { name: t("authSources:collapse", { name: "littleskin" }) })).toHaveAttribute("aria-expanded", "true");
    const main = page.getByRole("main");
    await main.evaluate((el) => { el.scrollTop = el.scrollHeight; });
    const save = page.getByRole("button", { name: t("authSources:save"), exact: true });
    await expect(save).toBeInViewport();
    const room = await save.evaluate((el) => el.closest("main")!.getBoundingClientRect().bottom - el.parentElement!.parentElement!.getBoundingClientRect().bottom);
    expect(room).toBeGreaterThanOrEqual(23);
    await expectFitsScreen(page);
  }
});

test("startup shows actual scheduling blockage, stale API state, then recovery", async ({ page, signIn }) => {
  await signIn("owner");
  let state: "blocked" | "outage" | "running" = "blocked";
  await page.route("**/api/v1/servers/survival/status", async (route) => {
    if (state === "outage") { await route.fulfill({ status: 503, json: { error: { code: "upstream_unavailable", message: "test API outage" } } }); return; }
    const original = await route.fetch();
    const data = await original.json();
    await route.fulfill({ json: { ...data, phase: state === "running" ? "Running" : "Starting", desiredState: "Running", ready: state === "running", startup: state === "blocked" ? { stage: "scheduling", reason: "Unschedulable", message: "0/1 nodes: Insufficient memory", startedAt: new Date(Date.now() - 60000).toISOString(), logsAvailable: false } : undefined } });
  });
  await page.goto("/servers/survival");
  await expect(page.getByText(t("servers:startup_scheduling"), { exact: true })).toBeVisible();
  await expect(page.getByText(t("servers:startup_memory"), { exact: true })).toBeVisible();
  await expect(page.getByText(t("servers:log_ended"), { exact: true })).toBeHidden();
  await expect(page.getByRole("button", { name: t("servers:retire_delete"), exact: true })).toBeHidden();
  state = "outage";
  await expect(page.getByText(t("servers:status_stale"), { exact: true })).toBeVisible({ timeout: 7000 });
  await expect(page.getByText(t("servers:startup_scheduling"), { exact: true })).toBeHidden();
  await expect(page.getByRole("button", { name: t("servers:stop"), exact: true })).toBeHidden();
  state = "running";
  await expect(page.getByText(t("servers:status_stale"), { exact: true })).toBeHidden({ timeout: 7000 });
  await page.getByRole("button", { name: t("servers:more_actions"), exact: true }).click();
  await expect(page.getByRole("button", { name: t("servers:retire_delete"), exact: true })).toBeVisible();
});

test("Owner emergency stop stays secondary, confirms name, and distinguishes acceptance from shutdown", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/servers/survival");
  await page.getByRole("button", { name: t("servers:more_actions"), exact: true }).click();
  await page.getByRole("button", { name: t("servers:emergency_stop"), exact: true }).click();
  const confirm = page.getByRole("button", { name: t("servers:emergency_stop"), exact: true });
  await expect(confirm).toBeDisabled();
  await page.getByLabel(t("servers:retire_confirm_label"), { exact: false }).fill("survival");
  await confirm.click();
  await expect(page.getByText(t("servers:emergency_stop_accepted"), { exact: true })).toBeVisible();
  await expect(confirm).toBeDisabled();
  await page.getByRole("dialog").getByRole("button", { name: t("common:close_sr"), exact: true }).last().click();
  await expectFitsScreen(page);
  await page.goto("/admin/platform");
  const ownerNav = page.getByRole("navigation").getByRole("link");
  const links = await ownerNav.evaluateAll((elements) => elements.map((el) => el.getAttribute("href")));
  expect(links.at(-1)).toBe("/admin/platform");
});

test("Owner executes node management and receives stage, failure logs and retry", async ({ page, signIn }) => {
  await signIn("owner");
  let task: Record<string, unknown> = {};
  let submitted: Record<string, unknown> | null = null;
  await page.route("**/api/v1/settings/node-control", (route) => route.fulfill({ json: { available: true, tasks: task.id ? [task] : [] } }));
  await page.route("**/api/v1/settings/node-control/tasks", async (route) => {
    submitted = route.request().postDataJSON();
    task = { id: "test-task", request: submitted, actor: "owner", state: "running", stage: "database_backup", startedAt: new Date().toISOString(), log: "[felis] database_backup" };
    await route.fulfill({ status: 202, json: task });
  });
  await page.route("**/api/v1/settings/node-control/tasks/test-task", (route) => route.fulfill({ json: task }));
  await page.route("**/api/v1/settings/node-control/tasks/test-task/retry", (route) => { task = { ...task, state: "running", error: undefined }; return route.fulfill({ status: 202, json: task }); });
  await page.goto("/admin/platform");
  await expect(page.getByLabel(t("admin:node_control_ip"))).toHaveCount(0);
  await page.getByRole("button", { name: t("admin:node_control_enable"), exact: true }).click();
  await page.getByLabel(t("admin:node_control_ip")).fill("192.0.2.10");
  const submit = page.getByRole("button", { name: t("admin:node_control_enable"), exact: true });
  await expect(submit).toBeDisabled();
  await page.getByLabel(t("admin:node_control_confirm_enable")).click();
  await submit.click();
  expect(submitted).toMatchObject({ action: "enable", externalIP: "192.0.2.10", confirmMaintenance: true });
  await expect(page.getByText(t("admin:node_control_stage_database_backup"), { exact: true })).toBeVisible();
  task = { ...task, state: "failed", error: "Database backup failed", log: "[felis] pg_dump failed: connection refused" };
  await page.getByRole("button", { name: t("admin:node_control_reload"), exact: true }).click();
  await expect(page.getByText("Database backup failed", { exact: true })).toBeVisible();
  await expect(page.getByLabel(t("admin:node_control_log"))).toContainText("pg_dump failed: connection refused");
  await page.getByRole("button", { name: t("admin:node_control_retry"), exact: true }).click();
  await expect(page.getByText(t("admin:node_control_state_running"), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
});

test("platform backup and version maintenance are Owner-only", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.patch("/api/v1/users/user", { data: { role: "admin" } })).ok()).toBe(true);
  await signIn("user");
  await page.goto("/admin/updates");
  await expect(page.getByRole("navigation").getByRole("link", { name: t("navigation:admin_updates"), exact: true })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: t("admin:updates_title"), exact: true })).toHaveCount(0);
  for (const path of ["updates/window", "updates/report", "platform/db-backup"]) {
    expect((await page.request.get(`/api/v1/${path}`)).status()).toBe(403);
  }
  await signIn("owner");
  await page.goto("/admin/updates");
  await expect(page.getByRole("heading", { name: t("admin:updates_title"), exact: true })).toBeVisible();
  await expect(page.getByRole("navigation").getByRole("link", { name: t("navigation:admin_updates"), exact: true })).toBeVisible();
});
