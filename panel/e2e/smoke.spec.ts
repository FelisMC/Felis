import { test, expect, t, expectFitsScreen } from "./fixtures";

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

test("an unlinked Owner can manage the panel, then preview and confirm a game role", async ({ page, signIn }) => {
  const profile = { source: "littleskin", name: "LemonMiaow", profile_uuid: "123456781234423482341234567890ab", mc_uuid: "canonical-role", auth_source: "thirdparty" };
  let linked = false;
  let designations = 0;
  await page.route("**/api/v1/account/link/start", (route) => route.fulfill({ json: { linked } }));
  await page.route("**/api/v1/account/link/sources", (route) => route.fulfill({ json: { sources: [{ tag: "littleskin", lookup_available: true }] } }));
  await page.route("**/api/v1/account/link/profile**", async (route) => {
    const request = route.request();
    if (request.method() === "POST") {
      expect(request.postDataJSON()).toEqual({ source: profile.source, profile_uuid: profile.profile_uuid });
      linked = true;
      designations++;
      await route.fulfill({ json: { linked: true, mc_uuid: profile.mc_uuid, auth_source: profile.auth_source } });
    } else {
      expect(new URL(request.url()).searchParams.get("profile")).toBe("LemonMiaow");
      await route.fulfill({ json: profile });
    }
  });
  await page.route("**/config.json", (route) => route.fulfill({ json: { apiBase: "/api/v1", rootDomain: "mc.example", gameVersion: "26.3", gamePort: 25570 } }));
  await signIn("owner");
  await page.goto("/admin/users");
  await expect(page.getByRole("heading", { name: t("admin:users_title") })).toBeVisible();

  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto("/account");
  await page.getByLabel(t("account:staff_profile")).fill("LemonMiaow");
  await page.getByRole("button", { name: t("account:staff_lookup") }).click();
  await expect(page.getByText(profile.profile_uuid)).toBeVisible();
  expect(designations).toBe(0);
  await expectFitsScreen(page);
  await page.getByRole("button", { name: t("account:staff_confirm") }).click();
  await expect(page.getByText(t("account:staff_linked_desc"))).toBeVisible();
  expect(designations).toBe(1);

  await page.getByText(t("account:game_guide"), { exact: true }).click();
  await expect(page.getByText(t("account:game_version", { version: "26.3" }))).toBeVisible();
  await expect(page.getByText("mc.example:25570", { exact: true })).toBeVisible();
  await expect(page.getByText(t("account:game_lobby"))).toBeVisible();
  await expectFitsScreen(page);
});

// Admin pages are chunks of their own, so a player never downloads them: every
// page module is fetched by its name (/src/pages/admin/UsersPage.tsx under the
// dev server, /assets/UsersPage-<hash>.js in a build).
const ADMIN_PAGE = /\/(UsersPage|UserDetailPage|LobbyPage|ImageAdmin|ImageBuildPage|SubmissionsPage|UpdatesPage)[.-]/;
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

test("on a wide screen the LuckPerms player list runs down its column", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/servers/survival/luckperms");
  const list = page.getByRole("button", { name: "mock_player", exact: true }).locator("..");
  await expect(list).toBeVisible();

  const listBox = (await list.boundingBox())!;
  const cardBox = (await list.locator("..").boundingBox())!;
  expect(cardBox.height).toBeGreaterThanOrEqual(500);
  expect(Math.abs(listBox.y + listBox.height - (cardBox.y + cardBox.height))).toBeLessThanOrEqual(2);

  // A taller screen grows the card to its cap, whatever the roster holds.
  await page.setViewportSize({ width: 1280, height: 1000 });
  await expect.poll(async () => (await list.locator("..").boundingBox())!.height).toBe(700);
});

test("staff customize the login space through the persistent file editor", async ({ page, signIn }) => {
  await signIn("owner");
  await page.goto("/admin/lobby?space=login");
  await expect(page.getByRole("heading", { name: t("lobby:title"), exact: true })).toBeVisible();
  const heading = page.getByLabel(t("lobby:bookHeading"));
  await expect(heading).toBeVisible();
  await heading.fill("Welcome to our network");
  await page.getByRole("button", { name: t("lobby:save"), exact: true }).click();
  await expect(page.getByText(t("lobby:saved"), { exact: true })).toBeVisible();
  await page.reload();
  await expect(heading).toHaveValue("Welcome to our network");
});

test("players cannot open the lobby administration page", async ({ page, signIn }) => {
  await signIn("linked");
  await page.goto("/admin/lobby");
  await expect(page.getByRole("heading", { name: t("lobby:title"), exact: true })).toHaveCount(0);
});
