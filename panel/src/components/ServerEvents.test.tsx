// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import i18next from "i18next";
import { formatAbsolute } from "@/lib/format";
import type { ServerEvent, ServerStatus } from "@/lib/types";
import { RecoveryNotice, ServerEventsCard } from "./ServerEvents";

const at = "2026-10-24T04:00:00Z";
const t = (key: string) => i18next.t(`servers:${key}`);
const stopped: ServerStatus = {
  name: "survival", subdomain: "survival", phase: "Stopped", desiredState: "Stopped", ready: false,
  playersOnline: 0, playersMax: 20, idleStopSeconds: 0, resumeAfterHostRestart: true,
  restartOnCrash: true, maxCrashRestarts: 3, notify: { started: false, stopped: false, problems: true },
  autoStop: { reason: "DiskFull", at }, diskFull: true,
  lastCrash: { at: "2026-10-23T00:00:00Z", reason: "OOMKilled", exitCode: 137 },
};

describe("disk quota activity", () => {
  it.each([
    { type: "DiskWarning", key: "event_disk_warning" },
    { type: "DiskFull", key: "event_disk_full" },
    { type: "Stopped", reason: "DiskFull", key: "event_stopped_disk" },
  ] as const)("explains $type without a generic stop or crash message", ({ key, ...event }) => {
    render(<ServerEventsCard events={[{ seq: 1, at, ...event } satisfies ServerEvent]} />);
    expect(screen.getByText(t(key))).toBeTruthy();
  });

  it("explains a quota stop using its own time and preserves the recovered state", () => {
    const { rerender } = render(<RecoveryNotice data={stopped} />);
    const notice = screen.getByRole("status", { name: t("autostop_disk_title") });
    expect(notice.textContent).toContain(t("disk_full_blocked"));
    expect(notice.textContent).not.toContain(t("crash_cause_oom"));
    expect(notice.querySelector("p[title]")?.getAttribute("title")).toBe(formatAbsolute(at, i18next.language));
    rerender(<RecoveryNotice data={{ ...stopped, diskFull: false }} />);
    expect(screen.getByText(t("autostop_disk_next"))).toBeTruthy();
    expect(screen.queryByText(t("disk_full_blocked"))).toBeNull();
  });
});
