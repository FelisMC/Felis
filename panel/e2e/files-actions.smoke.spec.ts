import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createHash } from "node:crypto";
import { test, expect, t, expectFitsScreen } from "./fixtures";

test("selected files copy into a folder, download as one archive and zip in place", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  const content = "file manager example\n";
  const created = await page.request.put("/api/v1/servers/lobby/file?path=notes.txt", { data: {
    content: Buffer.from(content).toString("base64"), content_sha256: createHash("sha256").update(content).digest("hex"), create_only: true,
  } });
  expect(created.ok()).toBe(true);
  expect((await page.request.post("/api/v1/servers/lobby/files/mkdir?path=archive")).ok()).toBe(true);
  await page.goto("/servers/lobby/files");
  await page.getByRole("checkbox", { name: t("files:select_item", { name: "notes.txt" }), exact: true }).check();
  await page.getByRole("checkbox", { name: t("files:select_item", { name: "eula.txt" }), exact: true }).check();
  await page.getByRole("button", { name: t("files:copy"), exact: true }).click();
  await page.getByRole("button", { name: t("files:open_folder", { name: "archive" }), exact: true }).click();
  await page.getByRole("button", { name: t("files:paste_count", { count: 2 }), exact: true }).click();
  await expect(page.getByRole("button", { name: t("files:open_file", { name: "notes.txt" }), exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: t("files:open_file", { name: "eula.txt" }), exact: true })).toBeVisible();
  await page.getByRole("checkbox", { name: t("files:select_all"), exact: true }).check();
  const downloaded = page.waitForEvent("download");
  await page.getByRole("button", { name: t("files:download_selected"), exact: true }).click();
  const download = await downloaded;
  expect(download.suggestedFilename()).toBe("lobby-files.zip");
  const zip = readFileSync((await download.path())!);
  expect(zip.readUInt32LE(0)).toBe(0x04034b50);
  expect(zip.includes(Buffer.from("notes.txt"))).toBe(true);
  expect(zip.includes(Buffer.from("eula.txt"))).toBe(true);
  expect(zip.includes(Buffer.from("archive/notes.txt"))).toBe(false);
  await page.getByRole("button", { name: t("files:zip_selected"), exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: t("files:zip_create"), exact: true }).click();
  await expect(page.getByRole("button", { name: t("files:open_file", { name: "archive.zip" }), exact: true })).toBeVisible();
  await expectFitsScreen(page);
});

test("folder selection uploads every file under its relative path", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  const temp = mkdtempSync(join(tmpdir(), "felis-upload-"));
  const directory = join(temp, "example-plugin");
  mkdirSync(join(directory, "settings"), { recursive: true });
  writeFileSync(join(directory, "settings", "config.yml"), "enabled: true\n");
  try {
    await page.goto("/servers/lobby/files");
    await page.getByTestId("folder-upload-input").setInputFiles(directory);
    await expect(page.getByRole("region", { name: t("files:uploads_label") }).getByText(t("files:upload_done"), { exact: true })).toBeVisible();
    await page.getByRole("button", { name: t("files:open_folder", { name: "example-plugin" }), exact: true }).click();
    await page.getByRole("button", { name: t("files:open_folder", { name: "settings" }), exact: true }).click();
    await expect(page.getByRole("button", { name: t("files:open_file", { name: "config.yml" }), exact: true })).toBeVisible();
    await expectFitsScreen(page);
  } finally {
    rmSync(temp, { recursive: true, force: true });
  }
});

test("small images preview through the download channel", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  const gif = Buffer.from("47494638396101000100800000000000ffffff2c00000000010001000002024401003b", "hex");
  expect((await page.request.put("/api/v1/servers/lobby/file?path=map.gif", { data: {
    content: gif.toString("base64"), content_sha256: createHash("sha256").update(gif).digest("hex"), create_only: true,
  } })).ok()).toBe(true);
  await page.goto("/servers/lobby/files");
  await page.getByRole("button", { name: t("files:open_file", { name: "map.gif" }), exact: true }).click();
  const image = page.getByRole("img", { name: t("files:image_preview", { path: "map.gif" }), exact: true });
  await expect(image).toBeVisible();
  await expect(image).toHaveJSProperty("naturalWidth", 1);
  await expectFitsScreen(page);
});
