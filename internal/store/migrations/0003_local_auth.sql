-- Phase B local-password auth + sessions + runtime settings.
-- The web (op.console) authenticates Owner/Operator via username+password;
-- `felis breakGlass` (the sudo-only emergency TUI) mints/resets these directly
-- against Postgres so recovery works even when the API is down. Players keep
-- password_hash NULL (account-link identity only — see account_links).

-- Owner/Operator credentials live on the existing users row, not a separate
-- table: role=admin WITH a hash is staff; role=user with NULL hash is a player.
ALTER TABLE users
  ADD COLUMN password_hash        text,                            -- bcrypt; NULL for link-only players
  ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;  -- force change-on-first-login

-- Server-set httpOnly session cookies. The remote path authenticates with a
-- stateless Cloudflare-Access JWT (no cookie); local-password auth needs its
-- own session. Store only the hash of the opaque cookie value, mirroring tokens.
CREATE TABLE sessions (
  token_hash text PRIMARY KEY,                          -- sha-256(cookie value)
  user_id    text NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz                                -- non-NULL once invalidated
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- Runtime security/platform settings as jsonb. The live API reads these from
-- Postgres per-request (NOT the read-only felis-config Secret), so the
-- break-glass TUI can flip toggles (e.g. local_auth_enabled) direct-to-DB
-- without patching the Secret and rolling the pod.
CREATE TABLE platform_settings (
  key        text PRIMARY KEY,                          -- e.g. 'local_auth_enabled'
  value      jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
