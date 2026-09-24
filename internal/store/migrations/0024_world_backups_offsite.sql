-- Off-site copies of world archives. offsite_at is when `felis offsite sync`
-- (felis-offsite.timer on the host) confirmed the archive's encrypted copy in
-- the [offsite] bucket; NULL means the archive exists on this node only. With
-- [offsite] configured the reaper deletes an idle world only once the archive
-- it made has an off-site copy, so losing the node's disk cannot take a reaped
-- world with it.
ALTER TABLE world_backups ADD COLUMN offsite_at timestamptz;

-- The sync's work list: present archives still waiting for their copy.
CREATE INDEX world_backups_offsite_pending_idx ON world_backups (created_at)
  WHERE status = 'present' AND offsite_at IS NULL;
