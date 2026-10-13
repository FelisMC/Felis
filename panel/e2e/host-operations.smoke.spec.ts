import { test, expect, t, expectFitsScreen } from "./fixtures";

// The Owner runs the host from the panel: status and the alert hold, backups, the
// off-site copy, diagnostics and the host settings, each through a host task.
test("Owner runs backups, diagnostics and the off-site copy from the maintenance page", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/updates");
  await expect(page.getByRole("heading", { name: t("admin:host_status_title"), exact: true })).toBeVisible();
  await expect(page.getByText("felis-db-backup.timer")).toBeVisible();
  await page.getByRole("button", { name: t("admin:host_quiet_hold"), exact: true }).click();
  await expect(page.getByRole("button", { name: t("admin:host_quiet_lift"), exact: true })).toBeVisible();
  await page.getByRole("button", { name: t("admin:host_quiet_lift"), exact: true }).click();
  await expect(page.getByRole("button", { name: t("admin:host_quiet_hold"), exact: true })).toBeVisible();

  await page.getByRole("tab", { name: t("admin:maintenance_tab_backups") }).click();
  await expect(page).toHaveURL(/tab=backups/);
  await page.getByRole("button", { name: t("admin:dbbackup_backup_now"), exact: true }).click();
  const backups = page.getByRole("tabpanel", { name: t("admin:maintenance_tab_backups") });
  await expect(backups.getByText(t("admin:host_action_db-backup"), { exact: true }).first()).toBeVisible();
  await expect(backups.getByText(t("admin:node_control_state_succeeded"), { exact: true }).first()).toBeVisible();
  await backups.getByRole("button", { name: t("admin:db_bundle_restore"), exact: true }).first().click();
  const restore = page.getByRole("dialog");
  await expect(restore.getByRole("button", { name: t("admin:db_bundle_restore"), exact: true })).toBeDisabled();
  await restore.getByRole("button", { name: t("common:cancel"), exact: true }).click();

  await page.getByRole("tab", { name: t("admin:maintenance_tab_diagnostics") }).click();
  await page.getByRole("button", { name: t("admin:doctor_run"), exact: true }).click();
  await expect(page.getByText(t("admin:doctor_area_alerts"), { exact: true })).toBeVisible();
  await expect(page.getByText(/no heartbeat URL is set/)).toBeVisible();
  await page.getByRole("button", { name: t("admin:support_create"), exact: true }).click();
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: t("admin:host_task_download"), exact: true }).click();
  expect((await download).suggestedFilename()).toMatch(/^felis-support-.*\.tar\.gz$/);

  await page.getByRole("tab", { name: t("admin:maintenance_tab_offsite") }).click();
  await expect(page.getByText(t("admin:offsite_not_configured"))).toBeVisible();
  await page.getByRole("button", { name: t("admin:offsite_configure"), exact: true }).click();
  await page.getByLabel(t("admin:offsite_endpoint"), { exact: true }).fill("https://s3.example.com");
  await page.getByLabel(t("admin:offsite_bucket"), { exact: true }).fill("felis-backup");
  await page.getByLabel(t("admin:offsite_access_key"), { exact: true }).fill("AKIA0");
  await page.getByLabel(t("admin:offsite_secret_key"), { exact: true }).fill("s3cret");
  await page.getByRole("button", { name: t("admin:offsite_save"), exact: true }).click();
  await page.getByRole("button", { name: t("admin:platform_reload"), exact: true }).click();
  await expect(page.getByText(t("admin:offsite_state_ok"), { exact: true })).toBeVisible();
  await page.getByRole("button", { name: t("admin:offsite_key_show"), exact: true }).click();
  await expect(page.getByText("bW9jay1vZmZzaXRlLWtleS1rZXB0LW9uLXRoZS1ob3N0IQ==")).toBeVisible();

  for (const viewport of [{ width: 1440, height: 1050 }, { width: 375, height: 812 }]) {
    await page.setViewportSize(viewport);
    await expectFitsScreen(page);
  }
});

test("Owner changes host settings and a server's forwarding from the panel", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/platform?tab=access");
  await expect(page.getByRole("heading", { name: t("admin:access_title"), exact: true })).toBeVisible();
  await page.getByLabel(t("admin:proxy_heap"), { exact: true }).fill("2G");
  await page.getByRole("button", { name: t("admin:proxy_apply"), exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: t("admin:proxy_apply"), exact: true }).click();
  await expect(page.getByText(t("admin:host_action_host-settings"), { exact: true })).toBeVisible();
  const credentials = page.getByRole("listitem").filter({ hasText: t("admin:credential_forwarding") });
  await credentials.getByRole("button", { name: t("admin:credentials_rotate"), exact: true }).click();
  await expect(page.getByRole("dialog").getByText(t("admin:credential_forwarding_impact"))).toBeVisible();
  await page.getByRole("dialog").getByRole("button", { name: t("common:cancel"), exact: true }).click();
  for (const viewport of [{ width: 1440, height: 1050 }, { width: 375, height: 812 }]) {
    await page.setViewportSize(viewport);
    await expectFitsScreen(page);
  }
  await page.setViewportSize({ width: 1440, height: 1050 });

  await page.goto("/servers/survival");
  await page.getByRole("button", { name: new RegExp(t("servers:forwarding_title")) }).click();
  await page.getByRole("switch", { name: t("servers:forwarding_use_legacy") }).click();
  await expect(page.getByText(t("servers:forwarding_risk"))).toBeVisible();
  await page.getByRole("button", { name: t("servers:forwarding_save"), exact: true }).click();
  await expect(page.getByRole("dialog").getByText(t("admin:node_control_state_succeeded"), { exact: true })).toBeVisible();
});

test("a non-Owner reaches none of the host's routes", async ({ page, signIn }) => {
  await signIn("user");
  for (const [method, path] of [["GET", "platform/host"], ["POST", "platform/host/quiet"], ["POST", "platform/offsite/key"], ["GET", "platform/audit-export"], ["GET", "settings/node-control"]] as const) {
    const res = method === "GET" ? await page.request.get(`/api/v1/${path}`) : await page.request.post(`/api/v1/${path}`, { data: {} });
    expect(res.status(), path).toBe(403);
  }
});
