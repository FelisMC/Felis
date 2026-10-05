// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SessionView } from "@/lib/types";
import { AccountSessionsCard } from "./AccountSessions";

const mocks = vi.hoisted(() => ({
  listMySessions: vi.fn(),
  revokeMySession: vi.fn(),
  revokeMyOtherSessions: vi.fn(),
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listMySessions: mocks.listMySessions,
      revokeMySession: mocks.revokeMySession,
      revokeMyOtherSessions: mocks.revokeMyOtherSessions,
    },
  };
});

const HOUR = 3_600_000;
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

function session(hash: string, userAgent: string, seenAgo: number, extra: Partial<SessionView> = {}): SessionView {
  return {
    token_hash: hash,
    created_at: ago(3 * 24 * HOUR),
    expires_at: new Date(Date.now() + 24 * HOUR).toISOString(),
    last_seen_at: ago(seenAgo),
    user_agent: userAgent,
    client_ip: "",
    ...extra,
  };
}

const mac = session(
  "h-mac",
  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
  // Whatever time is stored, the device reading the list is in use right now.
  10 * 60_000,
  { current: true, client_ip: "2001:db8::1" },
);
const iphone = session(
  "h-iphone",
  "Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
  20 * HOUR,
  { client_ip: "203.0.113.24" },
);
const windows = session(
  "h-windows",
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.2739.42",
  60_000,
);

function renderCard(props: Partial<Parameters<typeof AccountSessionsCard>[0]> = {}) {
  const onSignOut = vi.fn();
  render(<AccountSessionsCard version={0} staff={false} onSignOut={onSignOut} signingOut={false} {...props} />);
  return { onSignOut };
}

const row = (device: string) => screen.getByText(device).closest("li") as HTMLElement;

beforeEach(() => {
  mocks.listMySessions.mockReset();
  mocks.revokeMySession.mockReset();
  mocks.revokeMyOtherSessions.mockReset();
});

describe("AccountSessionsCard", () => {
  it("names each device and marks the one in use", async () => {
    mocks.listMySessions.mockResolvedValue([mac, iphone, windows]);
    renderCard();

    await screen.findByText("Chrome on macOS");
    const here = row("Chrome on macOS");
    expect(within(here).getByText("This device")).toBeTruthy();
    expect(within(here).getByText("Active now")).toBeTruthy();
    expect(within(here).getByText("2001:db8::1")).toBeTruthy();
    expect(within(here).getByText("Signed in 3 days ago")).toBeTruthy();
    expect(within(here).queryByRole("button")).toBeNull();

    const phone = row("Safari on iPhone");
    expect(within(phone).queryByText("This device")).toBeNull();
    expect(within(phone).getByText("Active 20 hours ago")).toBeTruthy();
    expect(within(phone).getByText("203.0.113.24")).toBeTruthy();
    expect(within(phone).getByRole("button", { name: "Sign out the session on Safari on iPhone" })).toBeTruthy();

    // Seen a minute ago: the server records activity once a minute, so that is now.
    expect(within(row("Edge on Windows")).getByText("Active now")).toBeTruthy();
    expect(screen.queryByText("Operations account sessions expire after 30 minutes of inactivity.")).toBeNull();
  });

  it("tells an operator that their sessions idle out", async () => {
    mocks.listMySessions.mockResolvedValue([mac]);
    renderCard({ staff: true });
    expect(
      await screen.findByText("Operations account sessions expire after 30 minutes of inactivity."),
    ).toBeTruthy();
  });

  it("signs one device out and drops it from the list", async () => {
    mocks.listMySessions.mockResolvedValueOnce([mac, iphone]).mockResolvedValue([mac]);
    mocks.revokeMySession.mockResolvedValue({ ok: true, signed_out: false });
    renderCard();

    await userEvent.click(await screen.findByRole("button", { name: "Sign out the session on Safari on iPhone" }));

    expect(mocks.revokeMySession).toHaveBeenCalledWith("h-iphone");
    expect((await screen.findByRole("status")).textContent).toBe("Signed out Safari on iPhone.");
    await waitFor(() => expect(screen.queryByText("Safari on iPhone")).toBeNull());
    expect(mocks.listMySessions).toHaveBeenCalledTimes(2);
  });

  it("counts a session that had already ended as signed out", async () => {
    mocks.listMySessions.mockResolvedValueOnce([mac, iphone]).mockResolvedValue([mac]);
    mocks.revokeMySession.mockRejectedValue({ status: 404, code: "session_not_found", message: "gone" });
    renderCard();

    await userEvent.click(await screen.findByRole("button", { name: "Sign out the session on Safari on iPhone" }));

    expect((await screen.findByRole("status")).textContent).toBe("Signed out Safari on iPhone.");
    expect(screen.queryByRole("alert")).toBeNull();
    await waitFor(() => expect(screen.queryByText("Safari on iPhone")).toBeNull());
  });

  it("says why signing a device out failed and keeps it listed", async () => {
    mocks.listMySessions.mockResolvedValue([mac, iphone]);
    mocks.revokeMySession.mockRejectedValue({ status: 403, code: "self_protected", message: "no" });
    renderCard();

    await userEvent.click(await screen.findByRole("button", { name: "Sign out the session on Safari on iPhone" }));

    expect((await screen.findByRole("alert")).textContent).toBe(
      "This operation is not permitted on the signed-in account.",
    );
    expect(screen.queryByRole("status")).toBeNull();
    await waitFor(() => expect(mocks.listMySessions).toHaveBeenCalledTimes(2));
    expect(screen.getByText("Safari on iPhone")).toBeTruthy();
  });

  it("signs every other device out after a confirmation", async () => {
    mocks.listMySessions.mockResolvedValueOnce([mac, iphone, windows]).mockResolvedValue([mac]);
    mocks.revokeMyOtherSessions.mockResolvedValue({ revoked: 2 });
    renderCard();

    await userEvent.click(await screen.findByRole("button", { name: "Sign out other devices" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText("Sign out all other device sessions?")).toBeTruthy();
    expect(mocks.revokeMyOtherSessions).not.toHaveBeenCalled();

    await userEvent.click(within(dialog).getByRole("button", { name: "Sign out other devices" }));

    expect(mocks.revokeMyOtherSessions).toHaveBeenCalledTimes(1);
    expect((await screen.findByRole("status")).textContent).toBe("Sessions on 2 other devices have been signed out.");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(screen.queryByText("Safari on iPhone")).toBeNull());
    expect(screen.getByText("Chrome on macOS")).toBeTruthy();
  });

  it("uses the singular for one device", async () => {
    mocks.listMySessions.mockResolvedValueOnce([mac, iphone]).mockResolvedValue([mac]);
    mocks.revokeMyOtherSessions.mockResolvedValue({ revoked: 1 });
    renderCard();

    await userEvent.click(await screen.findByRole("button", { name: "Sign out other devices" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Sign out other devices" }));

    expect((await screen.findByRole("status")).textContent).toBe("The session on 1 other device has been signed out.");
  });

  it("has nothing to sign out elsewhere when this is the only device", async () => {
    mocks.listMySessions.mockResolvedValue([mac]);
    renderCard();
    await screen.findByText("Chrome on macOS");
    expect(screen.getByRole("button", { name: "Sign out other devices" }).hasAttribute("disabled")).toBe(true);
  });

  it("signs this device out through the page's sign-out", async () => {
    mocks.listMySessions.mockResolvedValue([mac, iphone]);
    const { onSignOut } = renderCard();
    await screen.findByText("Chrome on macOS");

    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));

    expect(onSignOut).toHaveBeenCalledTimes(1);
    expect(mocks.revokeMySession).not.toHaveBeenCalled();
  });

  it("reloads when the page says the server changed the list", async () => {
    mocks.listMySessions.mockResolvedValueOnce([mac, iphone]).mockResolvedValue([mac]);
    const onSignOut = vi.fn();
    const { rerender } = render(
      <AccountSessionsCard version={0} staff={false} onSignOut={onSignOut} signingOut={false} />,
    );
    await screen.findByText("Safari on iPhone");

    rerender(<AccountSessionsCard version={1} staff={false} onSignOut={onSignOut} signingOut={false} />);

    await waitFor(() => expect(screen.queryByText("Safari on iPhone")).toBeNull());
    expect(mocks.listMySessions).toHaveBeenCalledTimes(2);
  });
});
