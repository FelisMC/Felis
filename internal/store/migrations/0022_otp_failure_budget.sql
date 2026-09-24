-- Account-level budget on wrong one-time codes. The per-code attempts cap
-- (email_otps.attempts) resets whenever a new code is minted, and the public
-- login door lets anyone mint one per minute, so on its own it allows about
-- 7,200 guesses a day against one account. This row survives supersede: it
-- counts every wrong code for a (user, purpose) inside a fixed window, and once
-- the budget is spent that door refuses even the right code until the window
-- ends (internal/api otpFailureBudget / otpFailureWindow).
CREATE TABLE otp_failure_windows (
  user_id      text        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  purpose      text        NOT NULL,
  window_start timestamptz NOT NULL,
  failures     int         NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, purpose)
);
