import { test, expect, t, expectFitsScreen } from "./fixtures";
import { createHash } from "node:crypto";

test("file editor highlights configuration, preserves CRLF, undoes edits and saves by shortcut", async ({ page, signIn }) => {
  await signIn("owner");
  await page.emulateMedia({ colorScheme: "dark" });
  await page.request.post("/api/v1/servers/lobby/stop");
  const original = "groups:\r\n  admin:\r\n    enabled: true\r\n    permissions:\r\n      - limboserver.stop\r\n      - limboserver.kick\r\n\r\nplayers:\r\n  LOOHP:\r\n    - admin\r\n";
  const created = await page.request.put("/api/v1/servers/lobby/file?path=permission.yml", { data: {
    content: Buffer.from(original).toString("base64"), content_sha256: createHash("sha256").update(original).digest("hex"), create_only: true,
  } });
  expect(created.ok()).toBe(true);
  await page.goto("/servers/lobby/files");
  await page.getByRole("button", { name: t("files:open_file", { name: "permission.yml" }), exact: true }).click();
  const editor = page.getByRole("textbox", { name: t("files:file_content") });
  await expect(editor).toBeVisible();
  await expect(page.getByRole("button", { name: t("files:save"), exact: true })).toBeDisabled();
  await expect(page.locator(".cm-lineNumbers .cm-gutterElement")).not.toHaveCount(0);
  await expect(page.locator(".cm-line span")).not.toHaveCount(0);
  await editor.press("ControlOrMeta+End");
  await page.keyboard.type("# edited");
  await editor.press("ControlOrMeta+z");
  await expect(editor).not.toContainText("# edited");
  await page.keyboard.type("# edited");
  await expectFitsScreen(page);
  await page.screenshot({ path: "/tmp/felis-files-editor.png", fullPage: true });
  const saved = page.waitForRequest((r) => r.method() === "PUT" && r.url().includes("/file?path="));
  await editor.press("ControlOrMeta+s");
  const body = (await saved).postDataJSON();
  expect(Buffer.from(body.content, "base64").toString()).toBe(original + "# edited");
  await expect(page.getByRole("dialog")).toHaveCount(0);
});

test("binary files show an explanation and download action without requesting text", async ({ page, signIn }) => {
  await signIn("owner");
  await page.emulateMedia({ colorScheme: "dark" });
  await page.request.post("/api/v1/servers/lobby/stop");
  let reads = 0;
  page.on("request", (r) => { if (r.url().includes("/file?path=")) reads++; });
  await page.goto("/servers/lobby/files");
  await page.getByRole("button", { name: t("files:open_folder", { name: "plugins" }), exact: true }).click();
  await page.getByRole("button", { name: t("files:open_file", { name: "LuckPerms-Bukkit-5.4.141.jar" }), exact: true }).click();
  await expect(page.getByText(t("files:binary_title"), { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: t("files:download_file"), exact: true })).toBeVisible();
  await expect(page.getByRole("textbox")).toHaveCount(0);
  expect(reads).toBe(0);
  await page.screenshot({ path: "/tmp/felis-files-binary.png", fullPage: true });
});

test("opening a slow file shows immediate progress and closing it ignores the delayed answer", async ({ page, signIn }) => {
  await signIn("owner");
  await page.emulateMedia({ colorScheme: "dark" });
  await page.request.post("/api/v1/servers/lobby/stop");
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/api/v1/servers/lobby/file?path=*", async (route) => { await pending; await route.continue(); });
  await page.goto("/servers/lobby/files");
  await page.getByRole("button", { name: t("files:open_file", { name: "server.properties" }), exact: true }).click();
  await expect(page.getByText(t("files:opening_file"), { exact: true })).toBeVisible();
  await page.screenshot({ path: "/tmp/felis-files-loading.png", fullPage: true });
  await page.keyboard.press("Escape");
  const answer = page.waitForResponse((r) => r.url().includes("/file?path="));
  release();
  await answer;
  await expect(page.getByRole("dialog")).toHaveCount(0);
});
