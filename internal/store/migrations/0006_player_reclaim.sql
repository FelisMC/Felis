-- Phase B3 player game-login: username-collision reclaim (Mojang-priority 正版优先).
--
-- The configured third-party Yggdrasil and the official Mojang service can both
-- mint the SAME username under DIFFERENT UUIDs. Velocity detects the collision,
-- routes the player to a limbo login server, and asks whether they own both
-- accounts. When they do NOT — i.e. the connecting player is NOT the genuine
-- Mojang owner — the genuine Mojang account reclaims the name: the squatter is
-- rejected and barred, and their world/player data is stashed for 30 days so a
-- new account can inherit it.
--
-- Velocity collision-routing, the limbo prompt, the authlib dual-backend, and the
-- data-inherit flow are CODE-ONLY (Java + a QR-bound device session a Postgres
-- row cannot express). THIS migration is the Go-verifiable data layer those
-- callbacks read and write: the reclaim event bars the squatter UUID and records
-- the hold; the login gate reads the blacklist to reject that UUID.

-- username_blacklist bars a non-genuine UUID from connecting after a reclaim. It
-- is keyed by mc_uuid, NEVER by the contested username: the genuine Mojang player
-- shares that name under a DIFFERENT UUID and must always pass. The block is on
-- the squatting identity, not the name — this is the load-bearing safety property
-- (正版优先 must never harm the genuine Mojang player).
CREATE TABLE username_blacklist (
  mc_uuid        uuid PRIMARY KEY,                          -- the barred (non-Mojang) UUID
  username       text NOT NULL,                             -- the contested name, for display/audit only
  reason         text NOT NULL DEFAULT 'mojang_priority_reclaim',
  blacklisted_at timestamptz NOT NULL DEFAULT now()
);

-- player_data_holds stashes the reclaimed (squatter) account's world/player data
-- so a new account can inherit it within the 30-day window (spec §B3 "数据将会被
-- 暂存 30 天"). The verifiable layer is WRITE-ONLY here: the reclaim event inserts
-- a row (held_at..expires_at, the opaque data_ref handle); the inherit/claim flow
-- that stamps reclaimed_by_user_id/reclaimed_at lives in the CODE-ONLY device-
-- session layer — a new account proves entitlement to an OLD uuid's data via its
-- QR-bound session, a primitive the Postgres layer cannot express, so those two
-- columns stay NULL throughout the verifiable path. UNIQUE (mc_uuid) makes a
-- repeated reclaim of an already-barred UUID idempotent (it should never recur —
-- the login gate rejects that UUID before it can reach reclaim again).
CREATE TABLE player_data_holds (
  id                   text PRIMARY KEY,                    -- opaque row id (crypto-random hex)
  mc_uuid              uuid NOT NULL UNIQUE,                -- the held (squatter) UUID
  username             text NOT NULL,                       -- the name at reclaim time
  data_ref             text,                                -- opaque archiver handle, server-side only (cf. world_backups.backup_ref); NULL until/unless archived
  held_at              timestamptz NOT NULL DEFAULT now(),
  expires_at           timestamptz NOT NULL,                -- held_at + 30d, set by the API clock (one authoritative clock)
  reclaimed_by_user_id text REFERENCES users(id),           -- NULL until a new account inherits the data (CODE-ONLY inherit flow)
  reclaimed_at         timestamptz                          -- NULL until inherited
);

-- The deferred inherit flow lists active holds by username (a reclaimed name's
-- new owner inheriting the old data), so index that lookup ahead of it.
CREATE INDEX player_data_holds_username_idx ON player_data_holds (username);
