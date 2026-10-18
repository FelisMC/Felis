-- Local-password auth, sessions and runtime settings for Owner and Operator accounts.
-- felis breakGlass resets these directly in Postgres; players keep password_hash NULL.

-- Owner/Operator credentials live on the existing users row, not a separate
-- table: role=admin WITH a hash is staff; role=user with NULL hash is a player.
ALTER TABLE users
  ADD COLUMN password_hash        text,                            -- bcrypt; NULL for link-only players
  ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;  -- force change-on-first-login

-- Session cookies are httpOnly; only the hash of the opaque value is stored, like tokens.
CREATE TABLE sessions (
  token_hash text PRIMARY KEY,                          -- sha-256(cookie value)
  user_id    text NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz                                -- non-NULL once invalidated
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- Runtime settings as jsonb, read per request from Postgres rather than the felis-config Secret, so break-glass can flip them without a pod roll.
CREATE TABLE platform_settings (
  key        text PRIMARY KEY,                          -- e.g. 'local_auth_enabled'
  value      jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
