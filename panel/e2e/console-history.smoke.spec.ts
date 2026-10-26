import { createHash } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, t, expectFitsScreen } from "./fixtures";

async function writeLog(page: Page, content: string) {
  expect((await page.request.put("/api/v1/servers/lobby/file?path=logs/latest.log", { data: {
    content: Buffer.from(content).toString("base64"), content_sha256: createHash("sha256").update(content).digest("hex"),
  } })).ok()).toBe(true);
}

test("saved logs and an unrelated player's sleeping server fit a narrow screen", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  await writeLog(page, Array.from({ length: 201 }, (_, index) => `old line ${index + 1}\n`).join(""));
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby");
  const log = page.getByRole("region", { name: t("servers:log_history_content"), exact: true });
  await expect(log.getByText("old line 201", { exact: true })).toBeVisible();
  await expectFitsScreen(page);
  await signIn("user");
  await page.goto("/servers/claim-me");
  await expect(page.getByText(t("servers:server_asleep_title"), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
});
