-- Phase B2 player onboarding: verified email via one-time code (OTP).
-- The forced web onboarding flow (link + email-OTP + passkey) needs to prove a
-- player controls an email address before it is bound to their account. A player
-- requests a code, the platform mails it, and the player types it back; only a
-- matching, unexpired, unconsumed code flips users.email_verified true and writes
-- the verified address onto the users row.

-- email_verified records that the address on the users row was proven via OTP, not
-- merely asserted. It defaults false so every existing (and link-only) row reads
-- unverified until a code is redeemed; the verify path is the only writer.
ALTER TABLE users
  ADD COLUMN email_verified boolean NOT NULL DEFAULT false;

-- One row per outstanding (and historical) email code. Only the sha-256 of the
-- code is stored, never the digits the player typed, so a database read cannot
-- replay a live code (same principle as sessions.token_hash). A code is single-use:
-- consumed_at is stamped the moment it is redeemed, and attempts caps brute force
-- against the short numeric keyspace independently of expiry.
CREATE TABLE email_otps (
  id         text PRIMARY KEY,                      -- opaque row id (random hex)
  user_id    text NOT NULL REFERENCES users(id),
  email      text NOT NULL,                         -- the address this code proves
  code_hash  text NOT NULL,                         -- sha-256(code); never the code
  purpose    text NOT NULL,                         -- e.g. 'onboard_email'
  attempts   int  NOT NULL DEFAULT 0,               -- wrong-guess counter, capped
  expires_at timestamptz NOT NULL,                  -- short TTL set by the API clock
  consumed_at timestamptz,                          -- non-NULL once redeemed (single-use)
  created_at timestamptz NOT NULL DEFAULT now()
);

-- The verify path looks up the newest live code for a (user, purpose), so index
-- that lookup. A user has at most one live code per purpose at a time (the mint
-- path supersedes the prior one), keeping this small.
CREATE INDEX email_otps_user_purpose_idx ON email_otps (user_id, purpose);
