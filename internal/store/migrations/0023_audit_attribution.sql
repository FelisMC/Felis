-- Audit attribution. actor is display text, and for a signed-in caller it used
-- to be the account's email, which a player can set to any unverified address,
-- so a row could name someone else. actor_user_id is the account that acted,
-- taken from the session (NULL for component actors and anonymous callers), and
-- client_ip / user_agent say where the call came from (client_ip is the
-- address the sign-in rate limit keys on: the edge's header when configured).
-- Users are soft-deleted, so the SET NULL only runs on a hard purge; actor
-- still names the account then.
ALTER TABLE audit_logs
  ADD COLUMN actor_user_id text REFERENCES users(id) ON DELETE SET NULL,
  ADD COLUMN client_ip     inet,
  ADD COLUMN user_agent    text;

CREATE INDEX audit_logs_actor_user_id_idx ON audit_logs (actor_user_id, created_at DESC)
  WHERE actor_user_id IS NOT NULL;
CREATE INDEX audit_logs_created_at_idx ON audit_logs (created_at DESC);
