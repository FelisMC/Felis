import { createHash } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, t, expectFitsScreen } from "./fixtures";

const propertiesURL = "/api/v1/servers/lobby/file?path=server.properties";
const sha = (text: string) => createHash("sha256").update(text).digest("hex");

async function writeProperties(page: Page, text: string) {
  const response = await page.request.put(propertiesURL, { data: { content: Buffer.from(text).toString("base64"), content_sha256: sha(text) } });
  expect(response.ok()).toBe(true);
}

test("the properties form fits a narrow screen", async ({ page, signIn }) => {
  await signIn("owner");
  await page.request.post("/api/v1/servers/lobby/stop");
  await writeProperties(page, "# Keep comments and order\r\nmax-players : 20\r\nmotd=First\\nSecond\r\ncustom-key=retained\r\nview-distance=10\r\nserver-port=25565\r\nonline-mode=false\r\nenable-rcon=true\r\nrcon.port=25575\r\nrcon.password=fixture-rcon\r\nforwarding-secrets=fixture-forwarding\r\n");
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby/files");
  await page.getByRole("button", { name: t("files:open_file", { name: "server.properties" }), exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("radio", { name: t("files:properties_form"), exact: true })).toHaveAttribute("aria-checked", "true");
  await expect(dialog.getByRole("textbox", { name: t("files:properties_field_max_players"), exact: true })).toHaveValue("20");
  await expectFitsScreen(page);
});
