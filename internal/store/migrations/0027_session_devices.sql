-- What a session list shows about each device. last_seen_at is when the session
-- last authenticated a request (the API advances it at most once a minute); a
-- staff session that sits idle past the idle limit stops authenticating.
-- user_agent and client_ip are recorded once, at sign-in. Existing rows count
-- as seen now, so the migration signs nobody out.
ALTER TABLE sessions
  ADD COLUMN last_seen_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN user_agent text NOT NULL DEFAULT '',
  ADD COLUMN client_ip text NOT NULL DEFAULT '';
