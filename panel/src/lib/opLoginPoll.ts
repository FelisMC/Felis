import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import type { ApiError } from "@/lib/types";

// The op-login status endpoint answers approved:false for an unknown, expired or
// used request alike (it must not tell a stranger which staff addresses exist), so
// it never says a request died. The expires_at that start returned is the only
// end the page gets.

/** OP_LOGIN_TTL_MS mirrors otpTTL in handlers_email_otp.go: how long a request lives. */
export const OP_LOGIN_TTL_MS = 10 * 60 * 1000;
export const OP_POLL_BASE_MS = 3000;
export const OP_POLL_MAX_MS = 30000;

/**
 * opLoginDeadline turns start's expires_at into a deadline on this browser's
 * clock. One already past, or further off than a request can live, means the two
 * clocks disagree; the request was minted just now, so the TTL from now is the
 * better estimate then.
 */
export function opLoginDeadline(expiresAt: string, now: number): number {
  const remaining = Date.parse(expiresAt) - now;
  if (!Number.isFinite(remaining) || remaining <= 0 || remaining > OP_LOGIN_TTL_MS) {
    return now + OP_LOGIN_TTL_MS;
  }
  return now + remaining;
}

/** opPollDelay doubles the wait after each consecutive failure, capped at OP_POLL_MAX_MS. */
export function opPollDelay(failures: number): number {
  return Math.min(OP_POLL_BASE_MS * 2 ** failures, OP_POLL_MAX_MS);
}

/** formatCountdown writes a remaining time as m:ss. */
export function formatCountdown(ms: number): string {
  const total = Math.ceil(Math.max(0, ms) / 1000);
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, "0")}`;
}

// A failure worth asking again after: no answer at all, a rate limit, or the server
// side. Anything else (local sessions switched off, a malformed id) answers the
// same way every time.
function retryable(e: unknown): boolean {
  const status = (e as Partial<ApiError> | null)?.status;
  return status === undefined || status === 0 || status === 429 || status >= 500;
}

export interface OpLoginPoll {
  approved: boolean;
  expired: boolean;
  remainingMs: number;
  /** The last status call failed and the next one waits longer. */
  retrying: boolean;
  /** A refusal that asking again will not change; polling has stopped. */
  error: unknown;
}

/**
 * useOpLoginPoll asks whether the request was approved in-game until it is, until
 * the deadline passes, or until the server refuses outright, backing off while the
 * calls fail. The countdown keeps running after approval: the code has to be
 * redeemed before the same deadline.
 */
export function useOpLoginPoll(requestId: string | null, deadline: number | null): OpLoginPoll {
  const [approved, setApproved] = useState(false);
  const [retrying, setRetrying] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    setApproved(false);
    setRetrying(false);
    setError(null);
    if (!requestId || deadline === null) return;
    let cancelled = false;
    let failures = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      if (Date.now() >= deadline) return;
      try {
        const s = await api.opLoginStatus(requestId);
        if (cancelled) return;
        failures = 0;
        setRetrying(false);
        if (s.approved) {
          setApproved(true);
          return;
        }
      } catch (e) {
        if (cancelled) return;
        if (!retryable(e)) {
          setRetrying(false);
          setError(e);
          return;
        }
        failures += 1;
        setRetrying(true);
      }
      timer = setTimeout(poll, opPollDelay(failures));
    };
    timer = setTimeout(poll, OP_POLL_BASE_MS);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [requestId, deadline]);

  useEffect(() => {
    if (deadline === null) return;
    setNow(Date.now());
    const tick = setInterval(() => {
      const n = Date.now();
      setNow(n);
      if (n >= deadline) clearInterval(tick);
    }, 1000);
    return () => clearInterval(tick);
  }, [deadline]);

  const remainingMs = deadline === null ? 0 : Math.max(0, deadline - now);
  return { approved, expired: deadline !== null && remainingMs === 0, remainingMs, retrying, error };
}
