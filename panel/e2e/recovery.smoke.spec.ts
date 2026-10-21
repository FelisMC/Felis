import { test, expect, t } from "./fixtures";

const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

test("an owner sees why their server stopped after crashing and sets how it recovers", async ({ page, signIn }) => {
  await signIn("user");
  await page.goto("/servers/creative");

  const notice = page.getByRole("status", { name: t("servers:autostop_crash_limit_title") });
  await expect(notice).toBeVisible();
  await expect(notice).toContainText(t("servers:crash_cause_oom"));
  await expect(notice).toContainText(t("servers:autostop_crash_next"));
  const activity = page.getByRole("region", { name: t("servers:events_title") });
  await expect(activity.getByRole("listitem").first()).toHaveText(new RegExp(escape(t("servers:event_stopped_crash"))));

  const open = async () => {
    await page.getByRole("button", { name: new RegExp(escape(t("servers:edit_server_title"))) }).click();
    return page.getByRole("dialog");
  };
  let dialog = await open();
  const resume = dialog.getByRole("switch", { name: new RegExp(escape(t("servers:edit_server_resume"))) });
  await expect(resume).toHaveAttribute("aria-checked", "true");
  await resume.click();
  await dialog.getByLabel(t("servers:edit_server_crash_max"), { exact: true }).fill("5");
  await dialog.getByRole("checkbox", { name: t("servers:edit_server_notify_started") }).check();
  await dialog.getByRole("button", { name: t("servers:edit_server_submit") }).click();
  await expect(dialog).toBeHidden();

  await page.reload();
  dialog = await open();
  await expect(dialog.getByRole("switch", { name: new RegExp(escape(t("servers:edit_server_resume"))) })).toHaveAttribute("aria-checked", "false");
  await expect(dialog.getByLabel(t("servers:edit_server_crash_max"), { exact: true })).toHaveValue("5");
  await expect(dialog.getByRole("checkbox", { name: t("servers:edit_server_notify_started") })).toBeChecked();
  await expect(dialog.getByRole("checkbox", { name: new RegExp(escape(t("servers:edit_server_notify_problems"))) })).toBeChecked();
  await page.keyboard.press("Escape");

  await page.getByRole("button", { name: t("servers:wake"), exact: true }).click();
  await expect(notice).toBeHidden();
});
