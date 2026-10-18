-- Mojang identities take priority during username-collision reclaim.

-- username_blacklist is keyed by mc_uuid, never by the username: the Mojang player shares the name under a different UUID and must always pass.
CREATE TABLE username_blacklist (
  mc_uuid        uuid PRIMARY KEY,                          -- the barred (non-Mojang) UUID
  username       text NOT NULL,                             -- the contested name, for display/audit only
  reason         text NOT NULL DEFAULT 'mojang_priority_reclaim',
  blacklisted_at timestamptz NOT NULL DEFAULT now()
);

-- player_data_holds keeps a reclaimed account's data for 30 days. reclaimed_by_user_id and reclaimed_at stay NULL here; the inherit flow runs in code.
CREATE TABLE player_data_holds (
  id                   text PRIMARY KEY,                    -- opaque row id (crypto-random hex)
  mc_uuid              uuid NOT NULL UNIQUE,                -- the held (squatter) UUID
  username             text NOT NULL,                       -- the name at reclaim time
  data_ref             text,                                -- opaque archiver handle, server-side only (cf. world_backups.backup_ref); NULL until/unless archived
  held_at              timestamptz NOT NULL DEFAULT now(),
  expires_at           timestamptz NOT NULL,                -- held_at + 30d, set by the API clock (one authoritative clock)
  reclaimed_by_user_id text REFERENCES users(id),           -- NULL until a new account inherits the data.
  reclaimed_at         timestamptz                          -- NULL until inherited
);

-- The deferred inherit flow lists active holds by username (a reclaimed name's
-- new owner inheriting the old data), so index that lookup ahead of it.
CREATE INDEX player_data_holds_username_idx ON player_data_holds (username);
