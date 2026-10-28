-- Personal API keys; only the SHA-256 of a key is stored, and an admin key must expire.
CREATE TABLE api_tokens (
  id           text PRIMARY KEY,
  user_id      text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         text NOT NULL,
  prefix       text NOT NULL,
  token_hash   text NOT NULL UNIQUE,
  admin        boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz,
  last_used_at timestamptz,
  revoked_at   timestamptz,
  CHECK (NOT admin OR expires_at IS NOT NULL)
);
CREATE INDEX api_tokens_user ON api_tokens (user_id, created_at DESC);
