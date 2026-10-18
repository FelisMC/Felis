-- Indexes for newest-first paging with id as tie-breaker, so a page reads only its own rows.
-- Covers the admin submission queue and the backups list.
CREATE INDEX image_submissions_created_idx ON image_submissions (created_at DESC, id DESC);
CREATE INDEX world_backups_present_idx ON world_backups (created_at DESC, id DESC)
  WHERE status = 'present';
CREATE INDEX world_backups_server_present_idx ON world_backups (server_name, created_at DESC, id DESC)
  WHERE status = 'present';
CREATE INDEX world_backups_former_owner_present_idx ON world_backups (former_owner, created_at DESC, id DESC)
  WHERE status = 'present';
