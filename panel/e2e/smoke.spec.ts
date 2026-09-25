import { test, expect, t } from "./fixtures";

test("a signed-out visit signs in by email code and returns to the page it asked for", async ({ page }) => {
  await page.goto("/servers");
  await expect(page).toHaveURL(/\/login\?next=%2Fservers$/);

  await page.getByLabel(t("auth:email_address")).fill("owner@mock.felis.local");
  await page.getByRole("button", { name: t("auth:send_otp") }).click();
  await page.getByLabel(t("auth:otp_code")).fill("123456");
  await page.getByRole("button", { name: t("auth:otp_btn") }).click();

  await expect(page).toHaveURL(/\/servers$/);
  await expect(page.getByRole("heading", { name: t("ops:fleet_title") })).toBeVisible();
});

test("a player sees their own server, can claim an unowned one, and has no admin section", async ({ page, signIn }) => {
  await signIn("linked");
  await page.goto("/servers");

  await expect(page.getByRole("heading", { name: t("servers:my_servers_title") })).toBeVisible();
  // Display name first, the server name beside it.
  const own = page.getByRole("row").filter({ hasText: "Hub Lobby" });
  await expect(own).toContainText("lobby");
  await expect(own.getByRole("link", { name: t("servers:console") })).toBeVisible();
  await expect(own.getByRole("button", { name: t("servers:stop"), exact: true })).toBeVisible();
  const unowned = page.getByRole("row").filter({ hasText: "Claimable Node" });
  await expect(unowned.getByRole("button", { name: t("servers:claim"), exact: true })).toBeVisible();
  await expect(unowned.getByRole("button", { name: t("servers:stop"), exact: true })).toHaveCount(0);
  await expect(page.getByRole("link", { name: t("navigation:admin_users") })).toHaveCount(0);

  await page.goto("/admin/images");
  await expect(page.getByText(t("common:not_authorized_title"))).toBeVisible();
});

test("stopping a server with players online asks first, and cancel keeps it running", async ({ page, signIn }) => {
  await signIn("linked");
  await page.goto("/servers/lobby");

  await page.getByRole("button", { name: t("servers:stop"), exact: true }).click();
  const question = page.getByText(t("servers:stop_confirm_players", { count: 28 }));
  await expect(question).toBeVisible();
  await page.getByRole("button", { name: t("common:cancel") }).click();
  await expect(question).toHaveCount(0);

  const status = await page.request.get("/api/v1/servers/lobby/status");
  expect((await status.json()).phase).toBe("Running");
});

test("an admin reaches the user list", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/users");

  await expect(page.getByRole("heading", { name: t("admin:users_title") })).toBeVisible();
  await expect(page.getByText("linked@mock.felis.local")).toBeVisible();
});

// Admin pages are chunks of their own, so a player never downloads them: every
// page module is fetched by its name (/src/pages/admin/UsersPage.tsx under the
// dev server, /assets/UsersPage-<hash>.js in a build).
const ADMIN_PAGE = /\/(UsersPage|UserDetailPage|ImageAdmin|ImageBuildPage|SubmissionsPage|UpdatesPage)[.-]/;
const ACCOUNT_PAGE = /\/Account[.-]/;

test("a player's pages load on demand and never pull in the admin pages", async ({ page, signIn }) => {
  const fetched: string[] = [];
  page.on("request", (req) => fetched.push(new URL(req.url()).pathname));
  await signIn("linked");
  await page.goto("/servers");
  await expect(page.getByRole("heading", { name: t("servers:my_servers_title") })).toBeVisible();
  expect(fetched.filter((p) => ACCOUNT_PAGE.test(p))).toEqual([]);

  await page.getByRole("link", { name: t("navigation:account"), exact: true }).click();
  await expect(page).toHaveURL(/\/account$/);
  await expect.poll(() => fetched.some((p) => ACCOUNT_PAGE.test(p))).toBe(true);
  expect(fetched.filter((p) => ADMIN_PAGE.test(p))).toEqual([]);
});
