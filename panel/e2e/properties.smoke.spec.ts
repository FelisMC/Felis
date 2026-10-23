import { createHash } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, t, expectFitsScreen } from "./fixtures";

const propertiesURL = "/api/v1/servers/lobby/file?path=server.properties";
const sha = (text: string) => createHash("sha256").update(text).digest("hex");

async function writeProperties(page: Page, text: string) {
  const response = await page.request.put(propertiesURL, { data: { content: Buffer.from(text).toString("base64"), content_sha256: sha(text) } });
  expect(response.ok()).toBe(true);
}

test("properties form preserves source, validates fields and saves shared edits with managed credentials intact", async ({ page, signIn }) => {
  await signIn("owner");
  await page.request.post("/api/v1/servers/lobby/stop");
  const source = "# Keep comments and order\r\nmax-players : 20\r\nmotd=First\\nSecond\r\ncustom-key=retained\r\nview-distance=10\r\nserver-port=25565\r\nonline-mode=false\r\nenable-rcon=true\r\nrcon.port=25575\r\nrcon.password=fixture-rcon\r\nforwarding-secrets=fixture-forwarding\r\n";
  await writeProperties(page, source);
  await page.goto("/servers/lobby/files");
  await page.getByRole("button", { name: t("files:open_file", { name: "server.properties" }), exact: true }).click();
  const dialog = page.getByRole("dialog");
  const form = dialog.getByRole("radio", { name: t("files:properties_form"), exact: true });
  const text = dialog.getByRole("radio", { name: t("files:properties_text"), exact: true });
  const save = dialog.getByRole("button", { name: t("files:save"), exact: true });
  await expect(form).toHaveAttribute("aria-checked", "true");
  await expect(save).toBeDisabled();
  await expect(dialog.getByRole("textbox", { name: t("files:properties_field_server_port"), exact: true })).not.toBeEditable();
  await expect(dialog.getByRole("textbox", { name: t("files:properties_field_rcon_password"), exact: true })).toHaveValue("<redacted by felis>");
  await text.click();
  await expect(dialog.getByRole("textbox", { name: t("files:file_content"), exact: true })).toContainText("custom-key=retained");
  await form.click();
  await expect(save).toBeDisabled();
  const maximum = dialog.getByRole("textbox", { name: t("files:properties_field_max_players"), exact: true });
  await maximum.fill("42");
  const viewDistance = dialog.getByRole("textbox", { name: t("files:properties_field_view_distance"), exact: true });
  await viewDistance.fill("33");
  await expect(dialog.getByText(t("files:properties_invalid_integer", { min: 3, max: 32 }), { exact: true })).toBeVisible();
  await expect(save).toBeDisabled();
  await viewDistance.fill("12");
  await text.click();
  await expect(dialog.getByRole("textbox", { name: t("files:file_content"), exact: true })).toContainText("max-players : 42");
  await form.click();
  await expect(maximum).toHaveValue("42");
  await page.setViewportSize({ width: 375, height: 812 });
  await expectFitsScreen(page);
  const request = page.waitForRequest((response) => response.method() === "PUT" && response.url().includes("/file?path=server.properties"));
  await maximum.press("ControlOrMeta+s");
  const body = (await request).postDataJSON();
  const expected = source.replace("max-players : 20", "max-players : 42").replace("view-distance=10", "view-distance=12");
  const redacted = expected.replace("fixture-rcon", "<redacted by felis>").replace("fixture-forwarding", "<redacted by felis>");
  expect(Buffer.from(body.content, "base64").toString()).toBe(redacted);
  expect(body.expect_sha256).toBe(sha(source));
  await expect(dialog).toHaveCount(0);
  const stored = await (await page.request.get(propertiesURL)).json();
  expect(stored.sha256).toBe(sha(expected));
  expect(Buffer.from(stored.content, "base64").toString()).toBe(redacted);
});

test("properties conflicts remain visible across modes and require reload or explicit overwrite", async ({ page, signIn }) => {
  await signIn("owner");
  await page.request.post("/api/v1/servers/lobby/stop");
  await writeProperties(page, "motd=original\nmax-players=20\n");
  await page.goto("/servers/lobby/files");
  await page.getByRole("button", { name: t("files:open_file", { name: "server.properties" }), exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("textbox", { name: t("files:properties_field_max_players"), exact: true }).fill("40");
  await writeProperties(page, "motd=external\nmax-players=21\n");
  await dialog.getByRole("button", { name: t("files:save"), exact: true }).click();
  await expect(dialog.getByText(t("files:conflict_title"), { exact: true })).toBeVisible();
  await dialog.getByRole("radio", { name: t("files:properties_text"), exact: true }).click();
  const raw = dialog.getByRole("textbox", { name: t("files:file_content"), exact: true });
  await expect(raw).toContainText("max-players=40");
  await expect(dialog.getByText(t("files:conflict_title"), { exact: true })).toBeVisible();
  await dialog.getByRole("button", { name: t("files:conflict_reload"), exact: true }).click();
  await expect(raw).toContainText("motd=external");
  await expect(dialog.getByText(t("files:conflict_title"), { exact: true })).toHaveCount(0);
  await expect(dialog.getByRole("button", { name: t("files:save"), exact: true })).toBeDisabled();
  await raw.press("ControlOrMeta+a");
  await page.keyboard.type("motd=my explicit replacement\nmax-players=50\n");
  await writeProperties(page, "motd=another external change\nmax-players=22\n");
  await raw.press("ControlOrMeta+s");
  await expect(dialog.getByText(t("files:conflict_title"), { exact: true })).toBeVisible();
  await dialog.getByRole("button", { name: t("files:conflict_overwrite"), exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const stored = await (await page.request.get(propertiesURL)).json();
  expect(Buffer.from(stored.content, "base64").toString()).toBe("motd=my explicit replacement\nmax-players=50\n");
});
