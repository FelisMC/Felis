-- Email OTP for player onboarding: a player proves an address by typing a one-time code.
-- Only a matching, unexpired, unconsumed code sets email_verified and writes the verified address.

-- email_verified is set only by the OTP verify path and defaults false.
ALTER TABLE users
  ADD COLUMN email_verified boolean NOT NULL DEFAULT false;

-- Only the sha-256 of each code is stored; attempts caps guesses against the short numeric keyspace.
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

-- The verify path reads the newest live code for (user, purpose); a mint supersedes the prior one.
CREATE INDEX email_otps_user_purpose_idx ON email_otps (user_id, purpose);
