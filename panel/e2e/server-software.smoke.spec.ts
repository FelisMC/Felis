import { test, expect, t } from "./fixtures";

const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

async function choose(page: import("@playwright/test").Page, field: string, option: string) {
  await page.getByRole("combobox", { name: field }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

test("a player creates a server of their own once the Owner opens self-service", async ({ page, signIn }) => {
  // Off on a new install: a player without a quota of their own is offered no form.
  await signIn("linked");
  await page.goto("/servers");
  await expect(page.getByRole("row").filter({ hasText: "Hub Lobby" })).toBeVisible();
  await expect(page.getByRole("button", { name: t("servers:create_server_btn") })).toHaveCount(0);
  const mine0 = (await (await page.request.get("/api/v1/me/servers")).json() as { servers: { owned: boolean }[] }).servers.filter((s) => s.owned).length;

  // Room for exactly one more server.
  await signIn("owner");
  await page.goto("/admin/users");
  await expect(page.getByText(t("admin:self_service_title"), { exact: true })).toBeVisible();
  await page.getByRole("switch", { name: new RegExp(escape(t("admin:self_service_enabled"))) }).click();
  await page.getByLabel(t("admin:self_service_servers"), { exact: true }).fill(String(mine0 + 1));
  await page.getByLabel(t("admin:self_service_memory"), { exact: true }).fill("8");
  await page.getByLabel(t("admin:self_service_storage"), { exact: true }).fill("30");
  await page.getByRole("button", { name: t("admin:self_service_save") }).click();
  await expect(page.getByText(t("admin:self_service_saved"), { exact: true })).toBeVisible();

  // Of the player's servers only the lobby has a size: 2 of the 8 GB and 10 of the 30.
  await signIn("linked");
  await page.goto("/servers");
  await page.getByRole("button", { name: t("servers:create_server_btn") }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText(t("servers:self_create_quota", { servers: `${mine0} / ${mine0 + 1}`, memory: "2048 / 8192 MB" }))).toBeVisible();
  await dialog.getByLabel(t("servers:create_server_name"), { exact: true }).fill("mine");
  await dialog.getByLabel(t("servers:create_server_subdomain"), { exact: true }).fill("mine");
  await dialog.getByRole("radio", { name: t("servers:software_type_paper"), exact: true }).click();
  await choose(page, t("servers:software_version"), "1.21.4");
  await expect(dialog.getByRole("combobox", { name: t("servers:software_build") })).toContainText("232");
  await expect(dialog.getByText(t("servers:software_forwarding_velocity"))).toBeVisible();
  await dialog.getByRole("checkbox", { name: t("servers:software_eula") }).check();
  await dialog.getByRole("button", { name: t("servers:create_server_submit"), exact: true }).click();
  await expect(dialog).toBeHidden();

  const mine = page.getByRole("row").filter({ hasText: "mine" });
  await expect(mine.getByRole("link", { name: t("servers:console") })).toBeVisible();
  // The quota is used up: the button stays, closed, and says why.
  await expect(page.getByRole("button", { name: t("servers:create_server_btn") })).toBeDisabled();
  await expect(page.getByRole("button", { name: t("servers:create_server_btn") })).toHaveAttribute("title", t("servers:self_create_quota_full"));

  await page.goto("/servers/mine");
  await expect(page.getByText(t("servers:software_card_title"), { exact: true })).toBeVisible();
  await expect(page.getByText(/Paper 1\.21\.4 232/)).toBeVisible();
  await expect(page.getByText(t("servers:software_identity_velocity"))).toBeVisible();
});

test("an owner moves their server to another version only after acknowledging a backup", async ({ page, signIn }) => {
  await signIn("linked");
  await page.goto("/servers/lobby");
  await expect(page.getByText(/Paper 1\.21\.4 232/)).toBeVisible();
  await page.getByRole("button", { name: t("servers:edit_server_title") }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: t("servers:edit_server_software_change") }).click();
  await choose(page, t("servers:software_version"), "1.12.2");
  await expect(dialog.getByText(t("servers:software_forwarding_none"))).toBeVisible();
  const save = dialog.getByRole("button", { name: t("servers:edit_server_submit"), exact: true });
  await expect(save).toBeDisabled();
  await dialog.getByRole("checkbox", { name: t("servers:edit_server_software_confirm") }).check();
  await save.click();
  await expect(dialog).toBeHidden();

  await page.reload();
  await expect(page.getByText(/Paper 1\.12\.2 1620/)).toBeVisible();
  await expect(page.getByText(t("servers:software_identity_none_badge"))).toBeVisible();
});

test("a custom jar chosen while creating a server is in its files before the first start", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/servers");
  await page.getByRole("button", { name: t("servers:create_server_btn") }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("radio", { name: t("servers:software_type_custom"), exact: true }).click();
  await dialog.getByLabel(t("servers:software_jar_file"), { exact: true }).setInputFiles({
    name: "server.jar", mimeType: "application/java-archive", buffer: Buffer.alloc(4096),
  });
  await expect(dialog.getByText(t("servers:software_jar_upload_to", { path: "server.jar" }))).toBeVisible();
  await dialog.getByLabel(t("servers:create_server_name"), { exact: true }).fill("hybrid");
  await dialog.getByLabel(t("servers:create_server_subdomain"), { exact: true }).fill("hybrid");
  await dialog.getByRole("checkbox", { name: t("servers:software_eula") }).check();
  await dialog.getByRole("button", { name: t("servers:create_server_submit"), exact: true }).click();
  await expect(dialog).toBeHidden({ timeout: 15_000 });

  await page.goto("/servers/hybrid/files");
  await expect(page.getByText("server.jar", { exact: true })).toBeVisible();
});

test("a user is held to the default quota until given one of their own, and back", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/users/linked");
  const own = page.getByRole("radio", { name: t("admin:user_quota_mode_custom") });
  const byDefault = page.getByRole("radio", { name: t("admin:user_quota_mode_default") });
  await expect(byDefault).toHaveAttribute("aria-checked", "true");
  await expect(page.getByText(t("admin:user_quota_default_off"))).toBeVisible();

  await own.click();
  await page.getByLabel(t("admin:self_service_servers"), { exact: true }).fill("4");
  await page.getByLabel(t("admin:self_service_memory"), { exact: true }).fill("12");
  await page.getByRole("button", { name: t("admin:user_quota_save") }).click();
  await expect(page.getByText(t("admin:user_quota_saved"), { exact: true })).toBeVisible();
  await page.reload();
  await expect(own).toHaveAttribute("aria-checked", "true");
  await expect(page.getByLabel(t("admin:self_service_memory"), { exact: true })).toHaveValue("12");

  await byDefault.click();
  await page.getByRole("button", { name: t("admin:user_quota_save") }).click();
  await expect(page.getByText(t("admin:user_quota_cleared"), { exact: true })).toBeVisible();
  await page.reload();
  await expect(byDefault).toHaveAttribute("aria-checked", "true");
});
