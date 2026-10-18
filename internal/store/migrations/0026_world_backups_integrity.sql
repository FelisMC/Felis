-- Record successful and failed archive verification separately; count entries omitted from archives.
ALTER TABLE world_backups
  ADD COLUMN sha256 text,
  ADD COLUMN verified_at timestamptz,
  ADD COLUMN corrupt_at timestamptz,
  ADD COLUMN skipped_entries integer NOT NULL DEFAULT 0;

-- The reaper's read-back work list: the present archives checked longest ago.
CREATE INDEX world_backups_verify_idx ON world_backups (verified_at NULLS FIRST, created_at)
  WHERE status = 'present' AND corrupt_at IS NULL;
