import { test, expect, t } from "./fixtures";

test("a running server accepts a file chosen in the file picker", async ({ page, signIn }) => {
  await signIn("owner");
  const properties = page.waitForResponse((response) => response.url().includes("/servers/survival/file?path=server.properties"));
  await page.goto("/servers/survival/files");
  await properties;
  await expect(page.getByText(t("files:live_warning"), { exact: true })).toBeVisible();
  await page.getByTestId("upload-input").setInputFiles({ name: "live-config.txt", mimeType: "text/plain", buffer: Buffer.from("enabled=true\n") });
  await expect(page.getByRole("button", { name: t("files:open_file", { name: "live-config.txt" }), exact: true })).toBeVisible();
});
