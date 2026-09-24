-- Integrity of world archives. sha256 is the digest of the archive as written
-- (NULL for archives written before digests were kept; the first read-back
-- records one). verified_at is when the reaper last read the archive back in
-- full and found it matching; corrupt_at is when a read-back failed, after
-- which the archive is never reused for a reap or offered for a restore.
-- skipped_entries counts the world entries the archive could not hold
-- (symbolic links, devices, sockets).
ALTER TABLE world_backups
  ADD COLUMN sha256 text,
  ADD COLUMN verified_at timestamptz,
  ADD COLUMN corrupt_at timestamptz,
  ADD COLUMN skipped_entries integer NOT NULL DEFAULT 0;

-- The reaper's read-back work list: the present archives checked longest ago.
CREATE INDEX world_backups_verify_idx ON world_backups (verified_at NULLS FIRST, created_at)
  WHERE status = 'present' AND corrupt_at IS NULL;
