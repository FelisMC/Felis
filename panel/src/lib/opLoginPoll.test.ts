import { describe, it, expect } from "vitest";
import { OP_LOGIN_TTL_MS, formatCountdown, opLoginDeadline, opPollDelay } from "./opLoginPoll";

const NOW = Date.parse("2026-09-25T12:00:00Z");

describe("opLoginDeadline", () => {
  it("counts down to the server's expires_at when the clocks agree", () => {
    expect(opLoginDeadline("2026-09-25T12:09:30Z", NOW)).toBe(NOW + 570_000);
  });

  it("falls back to the TTL from now when the clocks disagree", () => {
    // A browser clock running 20 minutes fast makes the request look dead before it is shown.
    expect(opLoginDeadline("2026-09-25T11:50:00Z", NOW)).toBe(NOW + OP_LOGIN_TTL_MS);
    // A browser clock an hour slow would make any request look like it lives longer than it can.
    expect(opLoginDeadline("2026-09-25T13:10:00Z", NOW)).toBe(NOW + OP_LOGIN_TTL_MS);
    expect(opLoginDeadline("not a time", NOW)).toBe(NOW + OP_LOGIN_TTL_MS);
  });
});

describe("opPollDelay", () => {
  it("asks every 3s while calls succeed and doubles per failure up to 30s", () => {
    expect([0, 1, 2, 3, 4, 10].map(opPollDelay)).toEqual([3000, 6000, 12000, 24000, 30000, 30000]);
  });
});

describe("formatCountdown", () => {
  it("writes minutes and zero-padded seconds, rounding a started second up", () => {
    expect(formatCountdown(600_000)).toBe("10:00");
    expect(formatCountdown(65_001)).toBe("1:06");
    expect(formatCountdown(9_000)).toBe("0:09");
    expect(formatCountdown(-5)).toBe("0:00");
  });
});
