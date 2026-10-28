// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import i18next from "i18next";
import { AuditPage } from "./AuditPage";
import type { AuditEntry } from "@/lib/types";

const calls = vi.hoisted(() => ({ listAudit: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, ...calls } };
});

const t = (key: string, options?: Record<string, unknown>) => i18next.t(key, { ns: "admin", ...options });

function entry(id: number, over: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id,
    created_at: "2026-10-01T00:00:00Z",
    actor: `actor${id}@example.net`,
    actor_user_id: null,
    source: "external",
    action: `server.act${id}`,
    server_name: "survival",
    request_id: null,
    client_ip: null,
    user_agent: null,
    payload: null,
    ...over,
  };
}

const fill = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const apply = () => fireEvent.click(screen.getByRole("button", { name: t("audit_filter_apply") }));

beforeEach(() => {
  calls.listAudit.mockReset();
});

describe("AuditPage", () => {
  it("sends the filters and restarts from the first page", async () => {
    calls.listAudit.mockResolvedValue({ entries: [entry(1)], next_cursor: "" });
    render(<AuditPage />);
    await screen.findByText("server.act1");

    fill(t("audit_filter_actor"), " alice ");
    fill(t("audit_filter_action"), "server.");
    fill(t("audit_filter_server"), "survival");
    fill(t("audit_filter_since"), "2026-10-01T08:00");
    fill(t("audit_filter_until"), "2026-10-02T08:00");
    fill(t("audit_filter_q"), "100%");
    apply();

    await waitFor(() => expect(calls.listAudit).toHaveBeenCalledTimes(2));
    expect(calls.listAudit).toHaveBeenLastCalledWith({
      actor: "alice",
      action: "server.",
      server: "survival",
      since: new Date(2026, 9, 1, 8, 0).toISOString(),
      until: new Date(2026, 9, 2, 8, 0).toISOString(),
      q: "100%",
      limit: 50,
    });
  });

  it("refuses an end time that does not follow the start time", async () => {
    calls.listAudit.mockResolvedValue({ entries: [entry(1)], next_cursor: "" });
    render(<AuditPage />);
    await screen.findByText("server.act1");

    fill(t("audit_filter_since"), "2026-10-02T08:00");
    fill(t("audit_filter_until"), "2026-10-01T08:00");

    expect(screen.getByText(t("audit_filter_order"))).toBeTruthy();
    expect((screen.getByRole("button", { name: t("audit_filter_apply") }) as HTMLButtonElement).disabled).toBe(true);
    expect(calls.listAudit).toHaveBeenCalledTimes(1);
  });

  it("appends the next page with the cursor of the last one", async () => {
    calls.listAudit
      .mockResolvedValueOnce({ entries: [entry(3), entry(2)], next_cursor: "2" })
      .mockResolvedValueOnce({ entries: [entry(1)], next_cursor: "" });
    render(<AuditPage />);
    await screen.findByText("server.act3");

    fireEvent.click(screen.getByRole("button", { name: t("audit_load_more") }));

    await screen.findByText("server.act1");
    expect(calls.listAudit).toHaveBeenLastCalledWith({ limit: 50, cursor: "2" });
    expect(screen.getByText("server.act3")).toBeTruthy();
    expect(screen.getByText("server.act2")).toBeTruthy();
    expect(screen.queryByRole("button", { name: t("audit_load_more") })).toBeNull();
    expect(screen.getByText(t("audit_end"))).toBeTruthy();
  });

  it("opens a record with the old and new value of each change", async () => {
    calls.listAudit.mockResolvedValue({
      entries: [
        entry(2, {
          action: "server.patch",
          payload: { image_from: "registry/paper:1.21.1", image_to: "registry/paper:1.21.4", fields: ["displayName", "idleStopSeconds"] },
        }),
        entry(1),
      ],
      next_cursor: "",
    });
    render(<AuditPage />);

    fireEvent.click(await screen.findByRole("button", { name: /server\.patch/ }));

    const dialog = within(screen.getByRole("dialog"));
    expect(dialog.getByText(t("audit_detail_change", { name: "image", from: "registry/paper:1.21.1", to: "registry/paper:1.21.4" }))).toBeTruthy();
    expect(dialog.getByText(t("audit_detail_fields", { fields: ["displayName", "idleStopSeconds"].join(t("audit_detail_separator")) }))).toBeTruthy();
    expect(dialog.getByText("actor2@example.net")).toBeTruthy();
  });
});
