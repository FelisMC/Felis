-- User management hardening: production-grade admin CRUD (spec §7 user admin).
-- Adds soft delete, disable toggle, audit timestamps, and an auto-updating
-- timestamp trigger so the admin user list reflects the last mutation without
-- every query re-deriving it from audit_logs.

-- Per-row lifecycle markers on the users table.
ALTER TABLE users
  ADD COLUMN disabled   boolean     NOT NULL DEFAULT false,
  ADD COLUMN deleted_at timestamptz,
  ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

-- updated_at auto-trigger, shared by any table that carries the column.
CREATE OR REPLACE FUNCTION felis_set_updated_at()
RETURNS trigger AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_users_updated_at
  BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION felis_set_updated_at();

-- quotas gains its own audit timestamp so an admin change (including who made it)
-- is visible downstream.
ALTER TABLE quotas
  ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN updated_by text;

CREATE TRIGGER trg_quotas_updated_at
  BEFORE UPDATE ON quotas
  FOR EACH ROW EXECUTE FUNCTION felis_set_updated_at();

-- Efficient lookups for the admin user list: filter by role and exclude
-- soft-deleted rows in one pass.
CREATE INDEX idx_users_role_active ON users (role)
  WHERE deleted_at IS NULL AND disabled = false;
CREATE INDEX idx_users_deleted ON users (deleted_at)
  WHERE deleted_at IS NOT NULL;
