import { createHash } from "node:crypto";
import type { Page } from "@playwright/test";
import { test, expect, t, expectFitsScreen } from "./fixtures";

const sha = (text: string) => createHash("sha256").update(text).digest("hex");
const fileURL = (path: string) => `/api/v1/servers/lobby/file?path=${encodeURIComponent(path)}`;
async function writeFile(page: Page, path: string, text: string) {
  expect((await page.request.put(fileURL(path), { data: { content: Buffer.from(text).toString("base64"), content_sha256: sha(text) } })).ok()).toBe(true);
}

test("YAML configuration forms fit a narrow screen", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.post("/api/v1/servers/lobby/stop")).ok()).toBe(true);
  const fixtures = [
    { path: "bukkit.yml", id: "bukkit_monsters",
      text: "# retained bukkit\r\nsettings:\r\n  allow-end: yes\r\nspawn-limits:\r\n  monsters: 70\r\ncustom-key: retained\r\n" },
    { path: "spigot.yml", id: "spigot_hopper_amount",
      text: "# retained spigot\nworld-settings:\n  default:\n    hopper-amount: 1\n    ticks-per:\n      hopper-amount: 99\n  my.world:\n    hopper-amount: 8\ncustom-key: retained\n" },
    { path: "config/paper-world-defaults.yml", id: "paper_thunder",
      text: "# retained paper\nenvironment:\n  disable-thunder: false\nchunks:\n  max-auto-save-chunks-per-tick: 24\ncustom-key: retained\n" },
    { path: "purpur.yml", id: "purpur_afk_away",
      text: "# retained purpur\nsettings:\n  messages:\n    afk-broadcast-away: '%s idle'\n    afk-broadcast-use-display-name: true\n  afk-broadcast-away: wrong path\ncustom-key: retained\n" },
  ];
  await page.setViewportSize({ width: 375, height: 812 });
  for (const fixture of fixtures) {
    await writeFile(page, fixture.path, fixture.text);
    await page.goto(fixture.path.startsWith("config/") ? "/servers/lobby/files?path=config" : "/servers/lobby/files");
    await page.getByRole("button", { name: t("files:open_file", { name: fixture.path.split("/").pop()! }), exact: true }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("radio", { name: t("files:properties_form"), exact: true })).toHaveAttribute("aria-checked", "true");
    await expect(dialog.getByRole(fixture.id === "paper_thunder" ? "combobox" : "textbox", { name: t(`files:config_field_${fixture.id}`), exact: true })).toBeVisible();
    await expectFitsScreen(page);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  }
});

test("protected Paper settings fit a narrow screen", async ({ page, signIn }) => {
  await signIn("owner");
  await writeFile(page, "server.properties", "level-name=config\n");
  await writeFile(page, "config/paper-global.yml", "# retained global\nproxies:\n  velocity:\n    enabled: true\n    secret: fixture-forwarding-secret\nchunk-loading-advanced:\n  auto-config-send-distance: true\ncollisions:\n  enable-player-collisions: yes\nwatchdog:\n  early-warning-delay: 10000\n  early-warning-every: 5000\nmessages:\n  no-permission: Before\ncustom-key: retained\n");
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby/files?path=config");
  await page.getByRole("button", { name: t("files:open_file", { name: "paper-global.yml" }), exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("heading", { name: t("files:paper_global_title"), exact: true })).toBeVisible();
  await expect(dialog.getByRole("textbox", { name: t("files:config_field_global_no_permission"), exact: true })).toBeVisible();
  await expectFitsScreen(page);
});
