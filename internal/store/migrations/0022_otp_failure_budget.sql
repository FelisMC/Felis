-- Account-level budget of wrong one-time codes per (user, purpose); per-code attempts reset with each new code, so this window is the real limit.
CREATE TABLE otp_failure_windows (
  user_id      text        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  purpose      text        NOT NULL,
  window_start timestamptz NOT NULL,
  failures     int         NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, purpose)
);
