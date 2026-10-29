import type { Page } from "@playwright/test";
import type { ServerExtraPort } from "../src/lib/types";
import { test, expect, t, expectFitsScreen } from "./fixtures";

const statusPath = (name: string) => `/api/v1/servers/${name}/status`;
const internalLabel = (number: number) => t("servers:extra_ports_internal", { number });
const protocolLabel = (number: number) => t("servers:extra_ports_protocol", { number });

async function appliedPorts(page: Page, name: string, count: number): Promise<ServerExtraPort[]> {
  let ports: ServerExtraPort[] = [];
  await expect.poll(async () => {
    const response = await page.request.get(statusPath(name));
    expect(response.ok()).toBe(true);
    const status = await response.json();
    ports = status.extraPorts ?? [];
    return !status.extraPortsPending && ports.length === count && ports.every((port) => !!port.address);
  }).toBe(true);
  return ports;
}

test("a server owner adds TCP and UDP ports and copies the direct address on a narrow screen", async ({ page, context, signIn }) => {
  await signIn("user");
  expect((await page.request.patch("/api/v1/me/servers/creative", { data: { extraPorts: [] } })).ok()).toBe(true);
  await page.setViewportSize({ width: 375, height: 812 });
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/servers/creative/settings");
  const ports = page.getByRole("region", { name: t("servers:settings_network_title"), exact: true });
  const add = ports.getByRole("button", { name: t("servers:extra_ports_add"), exact: true });
  await add.click();
  await ports.getByRole("combobox", { name: protocolLabel(1), exact: true }).click();
  await page.getByRole("option", { name: "UDP", exact: true }).click();
  await ports.getByRole("spinbutton", { name: internalLabel(1), exact: true }).fill("24454");
  await add.click();
  await ports.getByRole("spinbutton", { name: internalLabel(2), exact: true }).fill("8123");
  await expectFitsScreen(page);
  const bar = page.getByRole("region", { name: t("servers:settings_save_bar"), exact: true });
  await bar.getByRole("button", { name: t("servers:edit_server_submit"), exact: true }).click();
  await expect(bar).toHaveCount(0);
  const voice = (await appliedPorts(page, "creative", 2)).find((port) => port.protocol === "UDP")!;
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await ports.getByRole("button", { name: t("common:copy_address", { address: voice.address! }), exact: true }).click();
  await expect(ports.getByText(t("common:address_copied"), { exact: true })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(voice.address);
  await expectFitsScreen(page);
});
