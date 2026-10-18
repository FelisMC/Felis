-- When offsite storage is configured, require a confirmed archive copy before reaping its world.
ALTER TABLE world_backups ADD COLUMN offsite_at timestamptz;

-- The sync's work list: present archives still waiting for their copy.
CREATE INDEX world_backups_offsite_pending_idx ON world_backups (created_at)
  WHERE status = 'present' AND offsite_at IS NULL;
