import { test, expect, t, expectFitsScreen } from "./fixtures";

test("running servers accept uploads and require acknowledgement for world changes", async ({ page, signIn }) => {
  await signIn("owner");
  const properties = page.waitForResponse((response) => response.url().includes("/servers/survival/file?path=server.properties"));
  await page.goto("/servers/survival/files");
  await properties;
  await expect(page.getByText(t("files:live_warning"), { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: t("files:open_folder", { name: "world" }), exact: true })).toBeVisible();
  await page.getByTestId("upload-input").setInputFiles({ name: "live-config.txt", mimeType: "text/plain", buffer: Buffer.from("enabled=true\n") });
  await expect(page.getByRole("button", { name: t("files:open_file", { name: "live-config.txt" }), exact: true })).toBeVisible();
  await page.getByRole("button", { name: t("files:delete_item", { name: "world" }), exact: true }).click();
  const dialog = page.getByRole("dialog");
  const confirm = dialog.getByRole("button", { name: t("files:delete"), exact: true });
  await expect(confirm).toBeDisabled();
  await dialog.getByRole("checkbox", { name: t("files:live_world_confirm"), exact: true }).check();
  await expect(confirm).toBeEnabled();
  await expectFitsScreen(page);
  await dialog.getByRole("button", { name: t("common:cancel"), exact: true }).click();
});
