-- Audit attribution: actor_user_id is the acting session's account; client_ip and user_agent record where the call came from.
-- Users are soft-deleted, so SET NULL runs only on a hard purge.
ALTER TABLE audit_logs
  ADD COLUMN actor_user_id text REFERENCES users(id) ON DELETE SET NULL,
  ADD COLUMN client_ip     inet,
  ADD COLUMN user_agent    text;

CREATE INDEX audit_logs_actor_user_id_idx ON audit_logs (actor_user_id, created_at DESC)
  WHERE actor_user_id IS NOT NULL;
CREATE INDEX audit_logs_created_at_idx ON audit_logs (created_at DESC);
