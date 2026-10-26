import { createHash } from "node:crypto";
import { test, expect, t, expectFitsScreen } from "./fixtures";

test("a full disk still lets a file download through the browser on a narrow screen", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  const content = "download before cleanup\n";
  expect((await page.request.put("/api/v1/servers/lobby/file?path=quota-cleanup.txt", { data: {
    content: Buffer.from(content).toString("base64"), content_sha256: createHash("sha256").update(content).digest("hex"), create_only: true,
  } })).ok()).toBe(true);
  const disk = { usedBytes: 1200, limitBytes: 1000, sampledAt: "2026-10-23T00:00:00Z", stale: true };
  await page.route("**/api/v1/servers/lobby/status", async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response, json: { ...await response.json(), disk, diskFull: true,
      autoStop: { reason: "DiskFull", at: disk.sampledAt },
    } });
  });
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby/files");
  await expect(page.getByText(t("files:disk_cleanup_hint"), { exact: true })).toBeVisible();
  const downloaded = page.waitForEvent("download");
  await page.getByRole("button", { name: t("files:download_item", { name: "quota-cleanup.txt" }), exact: true }).click();
  expect((await downloaded).suggestedFilename()).toBe("quota-cleanup.txt");
  await expectFitsScreen(page);
});
