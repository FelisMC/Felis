import { createHash } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, t, expectFitsScreen } from "./fixtures";

const historyPath = "/api/v1/servers/lobby/console/history";

async function writeLog(page: Page, content: string) {
  expect((await page.request.put("/api/v1/servers/lobby/file?path=logs/latest.log", { data: {
    content: Buffer.from(content).toString("base64"), content_sha256: createHash("sha256").update(content).digest("hex"),
  } })).ok()).toBe(true);
}

test("a stopped console loads 200 saved lines at a time with formatting and mixed line endings", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  const lines = Array.from({ length: 450 }, (_, index) => {
    const text = index === 449 ? "§a[12:00:00 INFO] saved line 450 <b>plain markup</b>" : `saved line ${String(index + 1).padStart(3, "0")}`;
    return text + ["\r\n", "\n", "\r"][index % 3];
  });
  await writeLog(page, lines.join(""));
  const loaded = page.waitForResponse((response) => new URL(response.url()).pathname === historyPath);
  await page.goto("/servers/lobby");
  const response = await loaded;
  expect(response.ok()).toBe(true);
  expect(response.headers()["cache-control"]).toBe("no-store");
  const first = await response.json();
  const bytes = Buffer.from(first.content, "base64");
  expect(bytes.toString()).toBe(lines.slice(250).join(""));
  expect(first.content_sha256).toBe(createHash("sha256").update(bytes).digest("hex"));
  expect(first.has_more).toBe(true);
  expect(first.next_cursor.length).toBeLessThanOrEqual(256);
  const log = page.getByRole("region", { name: t("servers:log_history_content"), exact: true });
  await expect(log.getByText("saved line 251", { exact: true })).toBeVisible();
  await expect(log.getByText("saved line 250", { exact: true })).toHaveCount(0);
  await expect(log.getByText("[12:00:00 INFO] saved line 450 <b>plain markup</b>", { exact: true })).toHaveCSS("color", "rgb(85, 255, 85)");
  await expect(log.locator("b")).toHaveCount(0);
  await expect(page.getByRole("textbox", { name: t("servers:command_label"), exact: true })).toHaveCount(0);
  const more = page.getByRole("button", { name: t("servers:log_history_more"), exact: true });
  await more.click();
  await expect(log.getByText("saved line 051", { exact: true })).toBeVisible();
  await expect(log.getByText("saved line 050", { exact: true })).toHaveCount(0);
  await more.click();
  await expect(log.getByText("saved line 001", { exact: true })).toBeVisible();
  await expect(more).toHaveCount(0);
  await expectFitsScreen(page);
  await page.setViewportSize({ width: 375, height: 812 });
  await expectFitsScreen(page);
});

test("saved logs refresh after a file change and remain unavailable during startup or to an unrelated player", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  await writeLog(page, Array.from({ length: 201 }, (_, index) => `old line ${index + 1}\n`).join(""));
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby");
  const log = page.getByRole("region", { name: t("servers:log_history_content"), exact: true });
  await expect(log.getByText("old line 201", { exact: true })).toBeVisible();
  await writeLog(page, "replacement log\n");
  await page.getByRole("button", { name: t("servers:log_history_more"), exact: true }).click();
  await expect(page.getByRole("alert").getByText(t("servers:log_history_changed"), { exact: true })).toBeVisible();
  await expect(log.getByText("old line 201", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: t("servers:log_history_retry"), exact: true })).toHaveCount(0);
  const refresh = page.getByRole("button", { name: t("servers:log_history_refresh"), exact: true });
  await refresh.click();
  await expect(log.getByText("replacement log", { exact: true })).toBeVisible();
  await expect(log.getByText("old line 201", { exact: true })).toHaveCount(0);
  expect((await page.request.delete("/api/v1/servers/lobby/file?path=logs/latest.log")).ok()).toBe(true);
  await refresh.click();
  await expect(log.getByText(t("servers:log_history_empty"), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
  let reads = 0;
  page.on("request", (request) => { if (new URL(request.url()).pathname.endsWith("/console/history")) reads++; });
  await page.getByRole("button", { name: t("servers:wake"), exact: true }).click();
  await expect(log).toHaveCount(0);
  expect((await page.request.get(historyPath)).status()).toBe(409);
  expect(reads).toBe(0);
  await signIn("user");
  await page.goto("/servers/claim-me");
  await expect(page.getByText(t("servers:server_asleep_title"), { exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: t("servers:log_history_title"), exact: true })).toHaveCount(0);
  expect(reads).toBe(0);
  expect((await page.request.get("/api/v1/servers/claim-me/console/history")).status()).toBe(403);
  await expectFitsScreen(page);
});
