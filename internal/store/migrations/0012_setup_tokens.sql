-- One-time setup tokens for first web login: only the sha-256 of the token is stored; a token is single use and expires after 30 minutes.
CREATE TABLE setup_tokens (
  token_hash  text PRIMARY KEY,                                  -- sha-256(raw token); the raw value only ever lives in the URL
  user_id     text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at  timestamptz NOT NULL,                              -- redemption refused once passed (ConsumeSetupToken)
  consumed_at timestamptz,                                       -- non-NULL once redeemed; the single-use gate
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX setup_tokens_user_id_idx ON setup_tokens (user_id);
