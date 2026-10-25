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

test("a server owner saves TCP and UDP ports, copies the direct address and removes mappings on a narrow screen", async ({ page, context, signIn }) => {
  await signIn("user");
  expect((await page.request.patch("/api/v1/me/servers/creative", { data: { extraPorts: [] } })).ok()).toBe(true);
  await page.setViewportSize({ width: 375, height: 812 });
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  let powerRequests = 0;
  page.on("request", (request) => { if (request.method() === "POST" && /\/servers\/creative\/(wake|restart)$/.test(new URL(request.url()).pathname)) powerRequests++; });
  await page.goto("/servers/creative");
  await page.getByRole("button", { name: new RegExp(`^${t("servers:edit_server_title")}`) }).click();
  const dialog = page.getByRole("dialog");
  const add = dialog.getByRole("button", { name: t("servers:extra_ports_add"), exact: true });
  await add.click();
  await dialog.getByRole("combobox", { name: protocolLabel(1), exact: true }).click();
  await page.getByRole("option", { name: "UDP", exact: true }).click();
  await dialog.getByRole("spinbutton", { name: internalLabel(1), exact: true }).fill("24454");
  await add.click();
  await dialog.getByRole("spinbutton", { name: internalLabel(2), exact: true }).fill("8123");
  await expect(dialog.getByText(t("servers:extra_ports_edit_hint"), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
  const patch = page.waitForRequest((request) => request.method() === "PATCH" && new URL(request.url()).pathname === "/api/v1/me/servers/creative");
  await dialog.getByRole("button", { name: t("servers:edit_server_submit"), exact: true }).click();
  expect((await patch).postDataJSON()).toEqual({ extraPorts: [{ protocol: "UDP", port: 24454 }, { protocol: "TCP", port: 8123 }] });
  await expect(dialog).toHaveCount(0);
  const mapped = await appliedPorts(page, "creative", 2);
  const webMap = mapped.find((port) => port.protocol === "TCP" && port.port === 8123)!;
  const voice = mapped.find((port) => port.protocol === "UDP" && port.port === 24454)!;
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  const ports = page.getByRole("region", { name: t("servers:extra_ports_title"), exact: true });
  await expect(ports.getByText(voice.address!, { exact: true })).toBeVisible();
  await expect(ports.getByText(t("servers:extra_ports_external", { port: voice.nodePort! }), { exact: true })).toBeVisible();
  await ports.getByRole("button", { name: t("common:copy_address", { address: voice.address! }), exact: true }).click();
  await expect(ports.getByText(t("common:address_copied"), { exact: true })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(voice.address);
  await page.getByRole("button", { name: new RegExp(`^${t("servers:edit_server_title")}`) }).click();
  await dialog.getByRole("button", { name: t("servers:extra_ports_remove", { number: 1 }), exact: true }).click();
  await dialog.getByRole("button", { name: t("servers:edit_server_submit"), exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(await appliedPorts(page, "creative", 1)).toEqual([webMap]);
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await expect(ports.getByText(t("servers:extra_ports_mapping", { protocol: "UDP", port: 24454 }), { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: new RegExp(`^${t("servers:edit_server_title")}`) }).click();
  await dialog.getByRole("button", { name: t("servers:extra_ports_remove", { number: 1 }), exact: true }).click();
  const cleared = page.waitForRequest((request) => request.method() === "PATCH" && new URL(request.url()).pathname === "/api/v1/me/servers/creative");
  await dialog.getByRole("button", { name: t("servers:edit_server_submit"), exact: true }).click();
  expect((await cleared).postDataJSON()).toEqual({ extraPorts: [] });
  await expect(dialog).toHaveCount(0);
  await appliedPorts(page, "creative", 0);
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await expect(ports.getByText(t("servers:extra_ports_empty"), { exact: true })).toBeVisible();
  expect(powerRequests).toBe(0);
  await expectFitsScreen(page);
});

test("pending ports without a direct address remain editable by staff and private from other players", async ({ page, signIn }) => {
  await signIn("owner");
  expect((await page.request.patch("/api/v1/servers/lobby", { data: { extraPorts: [{ protocol: "TCP", port: 8123 }] } })).ok()).toBe(true);
  await page.route(`**${statusPath("lobby")}`, async (route) => {
    const response = await route.fetch();
    const status = await response.json();
    await route.fulfill({ response, json: { ...status, extraPortsPending: true,
      extraPorts: (status.extraPorts ?? []).map(({ protocol, port }: ServerExtraPort) => ({ protocol, port })),
    } });
  });
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/servers/lobby");
  const ports = page.getByRole("region", { name: t("servers:extra_ports_title"), exact: true });
  await expect(ports.getByText(t("servers:extra_ports_pending"), { exact: true })).toBeVisible();
  await expect(ports.getByText(t("servers:extra_ports_address_unavailable"), { exact: true })).toBeVisible();
  await expect(ports.getByRole("button")).toHaveCount(0);
  await page.getByRole("button", { name: new RegExp(`^${t("servers:edit_server_title")}`) }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("spinbutton", { name: internalLabel(1), exact: true })).toHaveValue("8123");
  await dialog.getByRole("combobox", { name: protocolLabel(1), exact: true }).click();
  await page.getByRole("option", { name: "UDP", exact: true }).click();
  await expectFitsScreen(page);
  const patch = page.waitForRequest((request) => request.method() === "PATCH" && new URL(request.url()).pathname === "/api/v1/servers/lobby");
  await dialog.getByRole("button", { name: t("servers:edit_server_submit"), exact: true }).click();
  expect((await patch).postDataJSON()).toEqual({ extraPorts: [{ protocol: "UDP", port: 8123 }] });
  await expect(dialog).toHaveCount(0);
  await expect(ports.getByText(t("servers:extra_ports_mapping", { protocol: "UDP", port: 8123 }), { exact: true })).toBeVisible();
  await expectFitsScreen(page);
  await signIn("user");
  await page.goto("/servers/lobby");
  await expect(page.getByRole("heading", { name: "Hub Lobby", exact: true })).toBeVisible();
  await expect(ports).toHaveCount(0);
  const publicStatus = await (await page.request.get(statusPath("lobby"))).json();
  expect(publicStatus).not.toHaveProperty("extraPorts");
  expect(publicStatus).not.toHaveProperty("extraPortsPending");
  expect((await page.request.patch("/api/v1/me/servers/lobby", { data: { extraPorts: [] } })).status()).toBe(403);
});
