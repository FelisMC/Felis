import { createHash } from "node:crypto";
import { test, expect, t, expectFitsScreen } from "./fixtures";

test("the console warns at 90 percent and keeps restart available", async ({ page, signIn }) => {
  await signIn("owner");
  const sampledAt = new Date().toISOString();
  await page.route("**/api/v1/servers/survival/status", async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...await response.json(), diskFull: false,
      disk: { usedBytes: 9 * 1024 ** 3, limitBytes: 10 * 1024 ** 3, sampledAt, stale: false },
    } });
  });
  await page.goto("/servers/survival");
  const disk = page.getByRole("region", { name: t("servers:disk_title"), exact: true });
  await expect(disk.getByText("9.0 GiB / 10 GiB", { exact: true })).toBeVisible();
  await expect(disk.getByText(t("servers:disk_warning"), { exact: true })).toBeVisible();
  await expect(disk.getByRole("meter")).toHaveAttribute("aria-valuenow", String(9 * 1024 ** 3));
  await expect(page.getByRole("button", { name: t("servers:restart"), exact: true })).toBeEnabled();
  await expectFitsScreen(page);
  await page.setViewportSize({ width: 375, height: 812 });
  await expectFitsScreen(page);
});

test("a full disk permits download and cleanup while startup waits for a fresh recovery sample", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  const content = "download before cleanup\n";
  expect((await page.request.put("/api/v1/servers/lobby/file?path=quota-cleanup.txt", { data: {
    content: Buffer.from(content).toString("base64"), content_sha256: createHash("sha256").update(content).digest("hex"), create_only: true,
  } })).ok()).toBe(true);
  let diskFull = true;
  let disk = { usedBytes: 1200, limitBytes: 1000, sampledAt: "2026-10-23T00:00:00Z", stale: true };
  await page.route("**/api/v1/servers/lobby/status", async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...await response.json(), disk, diskFull,
      autoStop: { reason: "DiskFull", at: disk.sampledAt },
    } });
  });
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby");
  const wake = page.getByRole("button", { name: t("servers:wake"), exact: true });
  await expect(wake).toBeDisabled();
  const usage = page.getByRole("region", { name: t("servers:disk_title"), exact: true });
  await expect(usage.getByText(t("servers:disk_stale"), { exact: false })).toBeVisible();
  await expect(usage.locator("time")).toHaveAttribute("datetime", disk.sampledAt);
  await expectFitsScreen(page);
  await page.goto("/servers/lobby/files");
  await expect(page.getByText(t("files:disk_cleanup_hint"), { exact: true })).toBeVisible();
  const downloaded = page.waitForEvent("download");
  await page.getByRole("button", { name: t("files:download_item", { name: "quota-cleanup.txt" }), exact: true }).click();
  expect((await downloaded).suggestedFilename()).toBe("quota-cleanup.txt");
  await page.getByRole("button", { name: t("files:delete_item", { name: "quota-cleanup.txt" }), exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: t("files:delete"), exact: true }).click();
  await expect(page.getByRole("button", { name: t("files:open_file", { name: "quota-cleanup.txt" }), exact: true })).toHaveCount(0);
  await expectFitsScreen(page);
  await page.goto("/servers/lobby");
  await expect(usage.getByText("1.2 KiB / 1000 B", { exact: true })).toBeVisible();
  disk = { ...disk, usedBytes: 100 };
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await expect(usage.getByText("100 B / 1000 B", { exact: true })).toBeVisible();
  await expect(wake).toBeDisabled();
  diskFull = false;
  disk = { ...disk, sampledAt: new Date().toISOString(), stale: false };
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await expect(wake).toBeEnabled();
  await expect(usage.getByText(t("servers:disk_stale"), { exact: false })).toHaveCount(0);
  const started = page.waitForResponse((response) => response.url().endsWith("/servers/lobby/wake") && response.request().method() === "POST");
  await wake.click();
  expect((await started).status()).toBe(202);
});
