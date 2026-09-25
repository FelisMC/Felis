-- The lists people page through read newest first with id breaking ties: the
-- admin submission queue across every user, and the backups list across every
-- server, one former owner's worlds, or one server's archives. Each index lets a
-- page read its own rows instead of sorting the whole table on every request.
CREATE INDEX image_submissions_created_idx ON image_submissions (created_at DESC, id DESC);
CREATE INDEX world_backups_present_idx ON world_backups (created_at DESC, id DESC)
  WHERE status = 'present';
CREATE INDEX world_backups_server_present_idx ON world_backups (server_name, created_at DESC, id DESC)
  WHERE status = 'present';
CREATE INDEX world_backups_former_owner_present_idx ON world_backups (former_owner, created_at DESC, id DESC)
  WHERE status = 'present';
