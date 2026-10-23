import { createHash } from "node:crypto";
import type { Page } from "@playwright/test";
import { parse } from "yaml";
import { test, expect, t, expectFitsScreen } from "./fixtures";

const sha = (text: string) => createHash("sha256").update(text).digest("hex");
const fileURL = (path: string) => `/api/v1/servers/lobby/file?path=${encodeURIComponent(path)}`;
const globalURL = "/api/v1/servers/lobby/config/paper-global";
async function writeFile(page: Page, path: string, text: string) {
  expect((await page.request.put(fileURL(path), { data: { content: Buffer.from(text).toString("base64"), content_sha256: sha(text) } })).ok()).toBe(true);
}

test("existing YAML forms share drafts with Text and save literal configuration paths with comments intact", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  const fixtures = [
    { path: "bukkit.yml", id: "bukkit_monsters", target: ["spawn-limits", "monsters"], value: 42,
      text: "# retained bukkit\r\nsettings:\r\n  allow-end: yes\r\nspawn-limits:\r\n  monsters: 70\r\ncustom-key: retained\r\n" },
    { path: "spigot.yml", id: "spigot_hopper_amount", target: ["world-settings", "default", "hopper-amount"], value: 2,
      text: "# retained spigot\nworld-settings:\n  default:\n    hopper-amount: 1\n    ticks-per:\n      hopper-amount: 99\n  my.world:\n    hopper-amount: 8\ncustom-key: retained\n" },
    { path: "config/paper-world-defaults.yml", id: "paper_thunder", target: ["environment", "disable-thunder"], value: true,
      text: "# retained paper\nenvironment:\n  disable-thunder: false\nchunks:\n  max-auto-save-chunks-per-tick: 24\ncustom-key: retained\n" },
    { path: "purpur.yml", id: "purpur_afk_away", target: ["settings", "messages", "afk-broadcast-away"], value: "%s away\nSecond line",
      text: "# retained purpur\nsettings:\n  messages:\n    afk-broadcast-away: '%s idle'\n    afk-broadcast-use-display-name: true\n  afk-broadcast-away: wrong path\ncustom-key: retained\n" },
  ];
  for (const fixture of fixtures) {
    await writeFile(page, fixture.path, fixture.text);
    await page.goto(fixture.path.startsWith("config/") ? "/servers/lobby/files?path=config" : "/servers/lobby/files");
    await page.getByRole("button", { name: t("files:open_file", { name: fixture.path.split("/").pop()! }), exact: true }).click();
    const dialog = page.getByRole("dialog");
    const form = dialog.getByRole("radio", { name: t("files:properties_form"), exact: true });
    const text = dialog.getByRole("radio", { name: t("files:properties_text"), exact: true });
    const save = dialog.getByRole("button", { name: t("files:save"), exact: true });
    await expect(form).toHaveAttribute("aria-checked", "true");
    await text.click();
    await expect(dialog.getByRole("textbox", { name: t("files:file_content"), exact: true })).toContainText("custom-key: retained");
    await form.click();
    await expect(save).toBeDisabled();
    const control = dialog.getByRole(typeof fixture.value === "boolean" ? "combobox" : "textbox", { name: t(`files:config_field_${fixture.id}`), exact: true });
    if (typeof fixture.value === "boolean") {
      await control.click();
      await page.getByRole("option", { name: t("files:properties_option_true"), exact: true }).click();
    } else {
      if (fixture.id === "bukkit_monsters") {
        await control.fill("1.5");
        await expect(dialog.getByText(t("files:config_invalid_integer"), { exact: true })).toBeVisible();
        await expect(save).toBeDisabled();
      }
      await control.fill(String(fixture.value));
    }
    await text.click();
    await form.click();
    if (typeof fixture.value !== "boolean") await expect(control).toHaveValue(String(fixture.value));
    await page.setViewportSize({ width: 375, height: 812 });
    await expectFitsScreen(page);
    const written = page.waitForRequest((request) => request.method() === "PUT" && new URL(request.url()).searchParams.get("path") === fixture.path);
    await control.press("ControlOrMeta+s");
    const body = (await written).postDataJSON();
    expect(body.expect_sha256).toBe(sha(fixture.text));
    const content = Buffer.from(body.content, "base64").toString();
    expect(content).toContain("# retained");
    const decoded = parse(content);
    expect(fixture.target.reduce((node, key) => node[key], decoded)).toBe(fixture.value);
    expect(decoded["custom-key"]).toBe("retained");
    if (fixture.path === "bukkit.yml") expect(content.replaceAll("\r\n", "")).not.toMatch(/[\r\n]/);
    if (fixture.path === "spigot.yml") {
      expect(decoded["world-settings"].default["ticks-per"]["hopper-amount"]).toBe(99);
      expect(decoded["world-settings"]["my.world"]["hopper-amount"]).toBe(8);
    }
    if (fixture.path === "purpur.yml") expect(decoded.settings["afk-broadcast-away"]).toBe("wrong path");
    await expect(dialog).toHaveCount(0);
  }
});

test("protected Paper settings keep secrets private and apply only acknowledged changes against the latest digest", async ({ page, signIn }) => {
  await signIn("owner");
  await writeFile(page, "server.properties", "level-name=config\n");
  const source = "# retained global\nproxies:\n  velocity:\n    enabled: true\n    secret: fixture-forwarding-secret\nchunk-loading-advanced:\n  auto-config-send-distance: true\ncollisions:\n  enable-player-collisions: yes\nwatchdog:\n  early-warning-delay: 10000\n  early-warning-every: 5000\nmessages:\n  no-permission: Before\ncustom-key: retained\n";
  await writeFile(page, "config/paper-global.yml", source);
  const initial = await (await page.request.get(globalURL)).json();
  expect(JSON.stringify(initial)).not.toContain("fixture-forwarding-secret");
  expect(initial.values).not.toHaveProperty("collisions.enable-player-collisions");
  expect((await page.request.get(fileURL("config/paper-global.yml"))).status()).toBe(400);
  expect((await page.request.post("/api/v1/servers/lobby/files/download?path=config/paper-global.yml")).status()).toBe(400);
  const patches: { values: Record<string, unknown>; expect_sha256: string }[] = [];
  page.on("request", (request) => { if (request.method() === "PATCH" && new URL(request.url()).pathname.endsWith("/config/paper-global")) patches.push(request.postDataJSON()); });
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby/files?path=config");
  await page.getByRole("button", { name: t("files:open_file", { name: "paper-global.yml" }), exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("heading", { name: t("files:paper_global_title"), exact: true })).toBeVisible();
  await expect(dialog.getByRole("radio", { name: t("files:properties_text"), exact: true })).toHaveCount(0);
  await expect(dialog.getByRole("combobox", { name: t("files:config_field_global_player_collisions"), exact: true })).toHaveCount(0);
  await expect(dialog.getByRole("textbox", { name: t("files:config_field_global_concurrent_loads"), exact: true })).toHaveCount(0);
  await expect(dialog.getByText("fixture-forwarding-secret", { exact: false })).toHaveCount(0);
  const message = dialog.getByRole("textbox", { name: t("files:config_field_global_no_permission"), exact: true });
  await message.fill("My form message");
  const save = dialog.getByRole("button", { name: t("files:save"), exact: true });
  await expect(save).toBeDisabled();
  await message.press("ControlOrMeta+s");
  expect(patches).toHaveLength(0);
  await dialog.getByRole("checkbox", { name: t("files:live_world_confirm"), exact: true }).check();
  const external = await page.request.patch(globalURL, { data: { values: { "watchdog.early-warning-every": 7500 }, expect_sha256: initial.sha256 } });
  expect(external.ok()).toBe(true);
  const latest = await external.json();
  await save.click();
  await expect(dialog.getByText(t("files:paper_global_conflict"), { exact: true })).toBeVisible();
  await expect(message).toHaveValue("My form message");
  await expectFitsScreen(page);
  await dialog.getByRole("button", { name: t("files:conflict_overwrite"), exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(patches).toEqual([
    { values: { "messages.no-permission": "My form message" }, expect_sha256: initial.sha256 },
    { values: { "messages.no-permission": "My form message" }, expect_sha256: latest.sha256 },
  ]);
  const stored = await (await page.request.get(globalURL)).json();
  expect(stored.values["messages.no-permission"]).toBe("My form message");
  expect(stored.values["watchdog.early-warning-every"]).toBe(7500);
  expect(JSON.stringify(stored)).not.toContain("fixture-forwarding-secret");
  const unchanged = await page.request.patch(globalURL, { data: { values: {}, expect_sha256: stored.sha256 } });
  expect(unchanged.ok()).toBe(true);
  expect((await unchanged.json()).sha256).toBe(stored.sha256);
  expect((await page.request.patch(globalURL, { data: { values: { "console.enable-brigadier-highlighting": true }, expect_sha256: stored.sha256 } })).status()).toBe(400);
  expect((await page.request.patch(globalURL, { data: { values: { "proxies.velocity.secret": "forbidden" }, expect_sha256: stored.sha256 } })).status()).toBe(400);
  await signIn("user");
  expect((await page.request.get("/api/v1/servers/claim-me/config/paper-global")).status()).toBe(403);
});
