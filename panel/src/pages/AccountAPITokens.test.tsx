// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import i18next from "i18next";
import type { APIToken, APITokenList } from "@/lib/types";
import { AccountAPITokensCard } from "./AccountAPITokens";

const mocks = vi.hoisted(() => ({
  listAPITokens: vi.fn(),
  createAPIToken: vi.fn(),
  revokeAPIToken: vi.fn(),
  reauthStatus: vi.fn(),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...mocks } };
});

const t = (key: string, opts?: Record<string, unknown>) => i18next.t(key, opts);

const HOUR = 3_600_000;
const DAY = 24 * HOUR;
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
const SECRET = "felis_pat_Zk3vQ8mT1nRw5yHb7cJd2sLf9gXe4uAa6pNo0iBqVtM";

function apiToken(id: string, name: string, extra: Partial<APIToken> = {}): APIToken {
  return {
    id,
    name,
    prefix: "felis_pat_Zk3vQ8mT",
    admin: false,
    created_at: ago(2 * DAY),
    expires_at: null,
    last_used_at: null,
    ...extra,
  };
}

function listOf(tokens: APIToken[], extra: Partial<APITokenList> = {}): APITokenList {
  return { enabled: true, limit: 10, can_create_admin: false, tokens, ...extra };
}

const row = (name: string) => screen.getByText(name).closest("li") as HTMLElement;
const createButton = () => screen.getByRole("button", { name: t("account:api_token_create") }) as HTMLButtonElement;

async function openCreateDialog(user: ReturnType<typeof userEvent.setup>) {
  await waitFor(() => expect(createButton().disabled).toBe(false));
  await user.click(createButton());
  return screen.findByRole("dialog");
}

async function submitName(user: ReturnType<typeof userEvent.setup>, dialog: HTMLElement, name: string) {
  await user.type(within(dialog).getByLabelText(t("account:api_token_name")), name);
  await user.click(within(dialog).getByRole("button", { name: t("account:api_token_create_submit") }));
}

beforeEach(() => {
  for (const fn of Object.values(mocks)) fn.mockReset();
});

describe("AccountAPITokensCard", () => {
  it("shows the key once after creating it and forgets it when the dialog is closed", async () => {
    const stored = apiToken("tok-1", "Deploy script");
    mocks.listAPITokens.mockResolvedValueOnce(listOf([])).mockResolvedValue(listOf([stored]));
    mocks.createAPIToken.mockResolvedValue({ ...stored, token: SECRET });
    const user = userEvent.setup();
    render(<AccountAPITokensCard />);

    await submitName(user, await openCreateDialog(user), "Deploy script");

    expect(await screen.findByText(SECRET)).toBeTruthy();
    expect(mocks.createAPIToken).toHaveBeenCalledWith(expect.objectContaining({ name: "Deploy script" }));
    expect(screen.getByText(t("account:api_token_created_warning"))).toBeTruthy();

    await user.click(screen.getByRole("button", { name: t("account:api_token_copy") }));
    expect(await screen.findByRole("button", { name: t("account:api_token_copied") })).toBeTruthy();
    expect(await navigator.clipboard.readText()).toBe(SECRET);

    await user.keyboard("{Escape}");
    expect(screen.getByText(SECRET)).toBeTruthy();

    await user.click(screen.getByRole("button", { name: t("account:api_token_saved") }));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText("felis_pat_Zk3vQ8mT…")).toBeTruthy();
    expect(document.body.textContent).not.toContain(SECRET);
  });

  it("retries the creation once the identity check passes", async () => {
    mocks.listAPITokens.mockResolvedValue(listOf([]));
    mocks.createAPIToken
      .mockRejectedValueOnce({ status: 403, code: "reauth_required", message: "confirm it's you first" })
      .mockResolvedValue({ ...apiToken("tok-1", "CI"), token: SECRET });
    mocks.reauthStatus.mockResolvedValue({ needed: false, factors: [] });
    const user = userEvent.setup();
    render(<AccountAPITokensCard />);

    await submitName(user, await openCreateDialog(user), "CI");

    expect(await screen.findByText(SECRET)).toBeTruthy();
    expect(mocks.createAPIToken).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("explains a refusal inside the dialog and keeps the form", async () => {
    mocks.listAPITokens.mockResolvedValue(listOf([]));
    mocks.createAPIToken.mockRejectedValue({ status: 409, code: "api_token_limit", message: "limit" });
    const user = userEvent.setup();
    render(<AccountAPITokensCard />);

    const dialog = await openCreateDialog(user);
    await submitName(user, dialog, "CI");

    expect((await within(dialog).findByRole("alert")).textContent).toBe(t("errors:api_token_limit"));
    expect((within(dialog).getByLabelText(t("account:api_token_name")) as HTMLInputElement).value).toBe("CI");
  });

  it("offers no admin key to an account that may not create one", async () => {
    mocks.listAPITokens.mockResolvedValue(listOf([], { can_create_admin: false }));
    const user = userEvent.setup();
    render(<AccountAPITokensCard />);

    const dialog = await openCreateDialog(user);

    expect(within(dialog).queryByRole("checkbox", { name: t("account:api_token_admin") })).toBeNull();
  });

  it("limits an admin key to 90 days and sends it as an admin key", async () => {
    mocks.listAPITokens.mockResolvedValue(listOf([], { can_create_admin: true }));
    mocks.createAPIToken.mockResolvedValue({ ...apiToken("tok-1", "Ops", { admin: true }), token: SECRET });
    const user = userEvent.setup();
    render(<AccountAPITokensCard />);

    const dialog = await openCreateDialog(user);
    const choices = () => within(dialog).getAllByRole("radio").map((radio) => radio.textContent);
    expect(choices()).toContain(t("account:api_token_expiry_never"));

    await user.click(within(dialog).getByRole("checkbox", { name: t("account:api_token_admin") }));
    expect(choices()).toEqual([
      t("account:api_token_expiry_7"),
      t("account:api_token_expiry_30"),
      t("account:api_token_expiry_90"),
    ]);

    await user.click(within(dialog).getByRole("radio", { name: t("account:api_token_expiry_90") }));
    await submitName(user, dialog, "Ops");
    await screen.findByText(SECRET);

    const body = mocks.createAPIToken.mock.calls[0][0];
    expect(body.admin).toBe(true);
    const remaining = Date.parse(body.expires_at) - Date.now();
    expect(remaining).toBeGreaterThan(89 * DAY);
    expect(remaining).toBeLessThanOrEqual(90 * DAY);
  });

  it("describes each key by its prefix, expiry and last use", async () => {
    mocks.listAPITokens.mockResolvedValue(
      listOf([
        apiToken("tok-1", "Deploy", { expires_at: new Date(Date.now() + 10 * DAY).toISOString(), last_used_at: ago(2 * HOUR) }),
        apiToken("tok-2", "Ops", { admin: true }),
      ]),
    );
    render(<AccountAPITokensCard />);

    await screen.findByText("Deploy");
    const deploy = within(row("Deploy"));
    expect(deploy.getByText("felis_pat_Zk3vQ8mT…")).toBeTruthy();
    expect(deploy.getByText(t("account:api_token_expires", { when: "in 10 days" }))).toBeTruthy();
    expect(deploy.getByText(t("account:api_token_last_used", { when: "2 hours ago" }))).toBeTruthy();
    expect(deploy.queryByText(t("account:api_token_admin_badge"))).toBeNull();

    const ops = within(row("Ops"));
    expect(ops.getByText(t("account:api_token_admin_badge"))).toBeTruthy();
    expect(ops.getByText(t("account:api_token_no_expiry"))).toBeTruthy();
    expect(ops.getByText(t("account:api_token_never_used"))).toBeTruthy();
  });

  it("keeps existing keys revocable while the platform has API keys off", async () => {
    mocks.listAPITokens.mockResolvedValue(listOf([apiToken("tok-1", "CI")], { enabled: false }));
    render(<AccountAPITokensCard />);

    expect(await screen.findByText(t("account:api_tokens_off"))).toBeTruthy();
    expect(createButton().disabled).toBe(true);
    const revoke = screen.getByRole("button", { name: t("account:api_token_revoke_aria", { name: "CI" }) });
    expect((revoke as HTMLButtonElement).disabled).toBe(false);
  });

  it("stops offering new keys at the limit", async () => {
    mocks.listAPITokens.mockResolvedValue(listOf([apiToken("tok-1", "CI"), apiToken("tok-2", "Backup")], { limit: 2 }));
    render(<AccountAPITokensCard />);

    expect(await screen.findByText(t("account:api_tokens_limit_reached", { limit: 2 }))).toBeTruthy();
    expect(createButton().disabled).toBe(true);
  });

  it("revokes a key only after the confirmation", async () => {
    mocks.listAPITokens
      .mockResolvedValueOnce(listOf([apiToken("tok-1", "CI"), apiToken("tok-2", "Backup")]))
      .mockResolvedValue(listOf([apiToken("tok-2", "Backup")]));
    mocks.revokeAPIToken.mockResolvedValue(undefined);
    render(<AccountAPITokensCard />);

    await userEvent.click(await screen.findByRole("button", { name: t("account:api_token_revoke_aria", { name: "CI" }) }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(t("account:api_token_revoke_desc"))).toBeTruthy();
    expect(mocks.revokeAPIToken).not.toHaveBeenCalled();

    await userEvent.click(within(dialog).getByRole("button", { name: t("account:api_token_revoke") }));

    expect(mocks.revokeAPIToken).toHaveBeenCalledWith("tok-1");
    expect((await screen.findByRole("status")).textContent).toBe(t("account:api_token_revoked", { name: "CI" }));
    await waitFor(() => expect(screen.queryByText("CI")).toBeNull());
    expect(screen.getByText("Backup")).toBeTruthy();
  });
});
