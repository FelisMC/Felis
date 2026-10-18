-- last_seen_at advances at most once a minute; user_agent and client_ip are recorded once at sign-in.
-- Existing rows count as seen now, so the migration signs nobody out.
ALTER TABLE sessions
  ADD COLUMN last_seen_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN user_agent text NOT NULL DEFAULT '',
  ADD COLUMN client_ip text NOT NULL DEFAULT '';
