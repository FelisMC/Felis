import { expect, test, t } from "./fixtures";
import type { Page } from "@playwright/test";

async function signInProvenOwner(page: Page, next = "/admin/platform") {
  await page.goto(next);
  await expect(page).toHaveURL(/\/login/);
  await page.getByLabel(t("auth:email_address")).fill("owner@mock.felis.local");
  await page.getByRole("button", { name: t("auth:send_otp") }).click();
  await page.getByLabel(t("auth:otp_code")).fill("123456");
  await page.getByRole("button", { name: t("auth:otp_btn") }).click();
  await expect(page).toHaveURL(new RegExp(next.replace(/\//g, "\\/")));
}

test("mode switching and customImages toggle update navigation, settings, and create dialog", async ({ page }) => {
  await signInProvenOwner(page, "/admin/platform");

  await expect(page.getByRole("link", { name: t("navigation:admin_images_hub"), exact: true })).toBeVisible();

  const customImagesSwitch = page.getByRole("switch", { name: t("admin:features_custom_images_label") });
  await expect(customImagesSwitch).toHaveAttribute("aria-checked", "true");
  await customImagesSwitch.click();
  await page.getByRole("button", { name: t("admin:features_save") }).click();
  await expect(page.getByText(t("admin:features_saved"))).toBeVisible();

  await expect(page.getByRole("link", { name: t("navigation:admin_images_hub"), exact: true })).toHaveCount(0);
  await page.goto("/admin/images");
  await expect(page.getByText(t("admin:images_hub_disabled_title"))).toBeVisible();
  await page.goto("/admin/builds");
  await expect(page.getByText(t("admin:images_hub_disabled_title"))).toBeVisible();

  await page.getByRole("link", { name: t("admin:images_hub_open_settings") }).click();
  await expect(page).toHaveURL(/\/admin\/platform$/);
  await page.getByRole("button", { name: new RegExp(t("admin:features_mode_team")) }).click();
  await page.getByRole("switch", { name: t("admin:features_custom_images_label") }).click();
  await page.getByRole("button", { name: t("admin:features_save") }).click();

  const confirmDialog = page.getByRole("dialog");
  await expect(confirmDialog).toContainText(t("admin:features_switch_title_team"));
  await confirmDialog.getByRole("button", { name: t("admin:features_switch_confirm") }).click();
  await expect(page.getByText(t("admin:features_saved"))).toBeVisible();
  await expect(page.getByRole("link", { name: t("navigation:admin_images_hub"), exact: true })).toBeVisible();
});

test("CreateServerDialog rejects preset tier buttons, parses free-text units, and derives slug from display name", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/servers");
  await page.getByRole("button", { name: t("servers:create_server_btn") }).click();

  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("button", { name: /^(2G|4G|8G|16G|2 GiB|4 GiB)$/i })).toHaveCount(0);

  await dialog.getByLabel(t("servers:create_server_title_name")).fill("Weekend Boss Event");
  await expect(dialog).toContainText("weekend-boss-event");

  await dialog.getByRole("radio", { name: /Paper/ }).click();
  await dialog.getByRole("combobox", { name: /Game version/i }).click();
  await page.getByRole("option").first().click();
  await dialog.getByLabel(t("servers:create_server_memory")).fill("4096 MiB");
  await dialog.getByLabel(t("servers:create_server_storage")).fill("24 GB");
  await dialog.getByRole("checkbox", { name: /End User License Agreement|EULA/i }).check();
  await dialog.getByRole("button", { name: t("servers:create_server_submit") }).click();

  await expect(dialog).toHaveCount(0);
  await page.getByPlaceholder(t("ops:fleet_search_placeholder")).fill("weekend-boss-event");
  await expect(page.getByRole("table").getByText("Weekend Boss Event")).toBeVisible();
});

test("team mode blocks non-collaborator users and grants dynamic access while collaborator row exists", async ({ page, context, baseURL }) => {
  await signInProvenOwner(page, "/admin/platform");

  await page.getByRole("button", { name: new RegExp(t("admin:features_mode_team")) }).click();
  await page.getByRole("button", { name: t("admin:features_save") }).click();
  await page.getByRole("dialog").getByRole("button", { name: t("admin:features_switch_confirm") }).click();
  await expect(page.getByText(t("admin:features_saved"))).toBeVisible();

  // Verify non-staff non-collaborator user is blocked with team_mode_blocked_notice.
  await context.addCookies([{ name: "felis_mock_session", value: "user", url: baseURL! }]);
  await page.goto("/servers");
  await expect(page).toHaveURL(/\/login/);
  await expect(page.getByText(t("auth:team_mode_blocked_notice"))).toBeVisible();

  // Owner adds user as an operator collaborator on survival.
  await context.addCookies([{ name: "felis_mock_session", value: "owner", url: baseURL! }]);
  await page.goto("/servers/survival/settings");
  await expect(page.getByText(t("servers:access_card_title"))).toBeVisible();
  await page.getByRole("combobox", { name: t("servers:collaborator_select_placeholder") }).click();
  await page.getByRole("option", { name: /user \(/ }).click();
  await page.getByRole("button", { name: t("servers:collaborator_add") }).click();
  await expect(page.getByText(t("servers:collaborator_saved", { name: "user" }))).toBeVisible();

  // Collaborator now accesses /servers and survival with operator tabs only.
  await context.addCookies([{ name: "felis_mock_session", value: "user", url: baseURL! }]);
  await page.goto("/servers");
  await expect(page.getByRole("heading", { name: t("servers:my_servers_title") })).toBeVisible();
  await expect(page.getByRole("table").getByText("Survival SMP")).toBeVisible();

  await page.goto("/servers/survival");
  const nav = page.getByRole("navigation", { name: t("servers:server_tabs") });
  await expect(nav.getByRole("link", { name: t("servers:tab_console") })).toBeVisible();
  await expect(nav.getByRole("link", { name: t("servers:tab_backups") })).toBeVisible();
  await expect(nav.getByRole("link", { name: t("servers:tab_files") })).toHaveCount(0);
  await expect(nav.getByRole("link", { name: t("servers:tab_settings") })).toHaveCount(0);

  await nav.getByRole("link", { name: t("servers:tab_backups") }).click();
  await expect(page.getByRole("button", { name: t("backups:backup_now") })).toBeVisible();
  await expect(page.getByRole("button", { name: t("backups:export_world") })).toHaveCount(0);
  await expect(page.getByRole("button", { name: t("backups:restore_btn_short") })).toHaveCount(0);

  // Removing collaborator immediately revokes panel access in team mode.
  await context.addCookies([{ name: "felis_mock_session", value: "owner", url: baseURL! }]);
  await page.goto("/servers/survival/settings");
  await page.getByRole("button", { name: t("servers:collaborator_remove_aria", { name: "user" }) }).click();
  await expect(page.getByText(t("servers:collaborator_removed", { name: "user" }))).toBeVisible();

  await context.addCookies([{ name: "felis_mock_session", value: "user", url: baseURL! }]);
  await page.goto("/servers");
  await expect(page).toHaveURL(/\/login/);
  await expect(page.getByText(t("auth:team_mode_blocked_notice"))).toBeVisible();
});

test("protected server isolates unauthorized admin to readonly and separates operator vs manager", async ({ page, context, baseURL }) => {
  await signInProvenOwner(page, "/servers/survival/settings");

  const protectSwitch = page.getByRole("switch", { name: t("servers:protected_toggle_label") });
  await protectSwitch.click();
  await expect(page.getByText(t("servers:protected_enabled_success"))).toBeVisible();

  // Unauthorized admin sees Protected badge and View status only.
  await context.addCookies([{ name: "felis_mock_session", value: "admin", url: baseURL! }]);
  await page.goto("/servers");
  await page.getByPlaceholder(t("ops:fleet_search_placeholder")).fill("survival");
  const survivalRow = page.locator("tr", { hasText: "Survival SMP" });
  await expect(survivalRow.getByText(t("servers:protected_badge"))).toBeVisible();
  await expect(survivalRow.getByRole("button", { name: t("servers:view_status") })).toBeVisible();
  await expect(survivalRow.getByRole("checkbox")).toHaveCount(0);

  await survivalRow.getByRole("button", { name: t("servers:view_status") }).click();
  await expect(page).toHaveURL(/\/servers\/survival$/);
  await expect(page.getByText(t("servers:protected_readonly_title"))).toBeVisible();
  await expect(page.getByText(t("servers:readonly_console_title"))).toBeVisible();
  await expect(page.getByRole("textbox", { name: t("servers:command_label") })).toHaveCount(0);

  const nav = page.getByRole("navigation", { name: t("servers:server_tabs") });
  await expect(nav.getByRole("link", { name: t("servers:tab_overview") })).toBeVisible();
  await expect(nav.getByRole("link", { name: t("servers:tab_files") })).toHaveCount(0);
  await expect(nav.getByRole("link", { name: t("servers:tab_settings") })).toHaveCount(0);

  await page.goto("/servers/survival/files");
  await expect(page.getByText(t("files:not_yours_title"))).toBeVisible();
  await page.goto("/servers/survival/settings");
  await expect(page.getByText(t("servers:settings_none_title"))).toBeVisible();

  // Owner grants admin operator collaborator access.
  await context.addCookies([{ name: "felis_mock_session", value: "owner", url: baseURL! }]);
  await page.goto("/servers/survival/settings");
  await page.getByRole("combobox", { name: t("servers:collaborator_select_placeholder") }).click();
  await page.getByRole("option", { name: /admin \(/ }).click();
  await page.getByRole("button", { name: t("servers:collaborator_add") }).click();
  await expect(page.getByText(t("servers:collaborator_saved", { name: "admin" }))).toBeVisible();

  await context.addCookies([{ name: "felis_mock_session", value: "admin", url: baseURL! }]);
  await page.goto("/servers/survival");
  await expect(page.getByRole("textbox", { name: t("servers:command_label") })).toBeVisible();
  await expect(nav.getByRole("link", { name: t("servers:tab_backups") })).toBeVisible();
  await expect(nav.getByRole("link", { name: t("servers:tab_files") })).toHaveCount(0);

  await page.goto("/servers/survival/files");
  await expect(page.getByText(t("files:not_yours_title"))).toBeVisible();

  // Owner upgrades admin collaborator to manager.
  await context.addCookies([{ name: "felis_mock_session", value: "owner", url: baseURL! }]);
  await page.goto("/servers/survival/settings");
  await page.getByRole("combobox", { name: t("servers:collaborator_role_for", { name: "admin" }) }).click();
  await page.getByRole("option", { name: t("servers:collaborator_role_manager") }).click();
  await expect(page.getByText(t("servers:collaborator_saved", { name: "admin" }))).toBeVisible();

  await context.addCookies([{ name: "felis_mock_session", value: "admin", url: baseURL! }]);
  await page.goto("/servers/survival/files");
  await expect(page.getByText(t("files:not_yours_title"))).toHaveCount(0);
  await page.goto("/servers/survival/backups");
  await expect(page.getByRole("button", { name: t("backups:restore_btn_short") }).first()).toBeVisible();

  // Manager on a protected server can edit settings but cannot manage collaborators or delete the server.
  await page.goto("/servers/survival/settings");
  await expect(page.getByRole("region", { name: t("servers:settings_section_general") })).toBeVisible();
  await expect(page.getByText(t("servers:access_card_title"))).toHaveCount(0);
  await expect(page.getByRole("button", { name: t("servers:retire_delete") })).toHaveCount(0);
});
