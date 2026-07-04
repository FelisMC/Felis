-- One-time setup tokens for the first-web-login bootstrap (spec §B).
-- `felis setup` binds the Owner's Minecraft account, promotes it to the
-- passwordless Owner (role='admin'), and mints one of these — the raw token
-- rides in the op.console /setup?token=... URL while only its sha-256 hash is
-- stored here, mirroring sessions and account_link_codes. Opening the URL
-- redeems the token once (ConsumeSetupToken) for a lockdown session in which the
-- Owner verifies their email and enrols a passkey instead of setting a password.
-- Tokens are single-use (consumed_at) and short-lived (expires_at, 30 min); a
-- redeemed row is spent, not deleted, so a replayed URL is a clean miss rather
-- than a fresh mint. ON DELETE CASCADE keeps pending tokens from outliving the
-- user they bootstrap (mirrors webauthn_credentials, migration 0008).
CREATE TABLE setup_tokens (
  token_hash  text PRIMARY KEY,                                  -- sha-256(raw token); the raw value only ever lives in the URL
  user_id     text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at  timestamptz NOT NULL,                              -- redemption refused once passed (ConsumeSetupToken)
  consumed_at timestamptz,                                       -- non-NULL once redeemed; the single-use gate
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX setup_tokens_user_id_idx ON setup_tokens (user_id);
