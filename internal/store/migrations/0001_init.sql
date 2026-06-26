-- Felis business-layer schema (spec §6). The CRD is the lifecycle
-- source-of-truth; this database owns only what the CRD cannot express:
-- ownership/claim, account links, quotas, image admission, builds, backups,
-- audit. Authoritative CRD fields are never duplicated here.

CREATE TYPE user_role        AS ENUM ('admin','user');
CREATE TYPE build_status     AS ENUM ('pending','building','succeeded','failed','cancelled');
CREATE TYPE backup_status    AS ENUM ('present','expired','deleted');

CREATE TABLE users (
  id text PRIMARY KEY, username text UNIQUE NOT NULL, email text,
  role user_role NOT NULL DEFAULT 'user', created_at timestamptz NOT NULL DEFAULT now()
);

-- Identity bridge: web identity <-> MC UUID (claim/owner-only/allowlist rely on it).
CREATE TABLE account_links (
  user_id text NOT NULL REFERENCES users(id), mc_uuid uuid NOT NULL,
  verified_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, mc_uuid), UNIQUE (mc_uuid)
);
CREATE TABLE account_link_codes ( code text PRIMARY KEY, mc_uuid uuid NOT NULL, expires_at timestamptz NOT NULL );

CREATE TABLE quotas (
  user_id text PRIMARY KEY REFERENCES users(id),
  max_servers int, max_cpu_milli int, max_memory_mb int, max_storage_gb int
);

-- Business projection: the CRD lives in K8s; this stores only the
-- ownership/activity/warning that the CRD cannot express, plus a fast-query cache.
CREATE TABLE servers (
  name           text PRIMARY KEY,           -- matches CRD metadata.name
  owner_id       text REFERENCES users(id),  -- NULL until claimed; reaper resets to NULL
  claimed_at     timestamptz,
  last_active_at timestamptz NOT NULL DEFAULT now(),  -- max(last human join, created_at)
  warned_3d_at   timestamptz, warned_1d_at timestamptz,  -- reaper warning dedup; cleared on renewal
  cached_phase   text,                       -- CRD status projection, non-authoritative
  created_at     timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz
);
CREATE TABLE server_aliases ( subdomain text PRIMARY KEY, server_name text NOT NULL REFERENCES servers(name) );
CREATE TABLE server_allowlist (             -- autostartPolicy=allowlist; first join auto-appends
  server_name text NOT NULL REFERENCES servers(name), mc_uuid uuid NOT NULL,
  added_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (server_name, mc_uuid)
);

-- Image admission (dynamic, auditable -> DB, not toml).
CREATE TABLE image_whitelist (
  image_ref text PRIMARY KEY,               -- registry/foo:1.0 or registry/foo:*
  source text NOT NULL DEFAULT 'built',     -- built (cluster build) | external (pushed)
  build_id text, added_by text NOT NULL, enabled boolean NOT NULL DEFAULT true,
  added_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE image_builds (
  id text PRIMARY KEY, image_ref text NOT NULL, status build_status NOT NULL DEFAULT 'pending',
  dockerfile text NOT NULL,                  -- archived for audit
  context_ref text, base_image text,         -- resolved FROM, audit
  requested_by text NOT NULL, job_name text, log_ref text, error text,
  created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz
);

-- World backups (reaper output; not FK'd to servers, which may be reset/deleted).
CREATE TABLE world_backups (
  id text PRIMARY KEY, server_name text NOT NULL, former_owner text,
  backup_ref text NOT NULL,                  -- WorldArchiver location (ArchiveRef)
  size_bytes bigint, reason text NOT NULL,   -- inactive_15d | manual
  status backup_status NOT NULL DEFAULT 'present',
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,           -- created_at + 3mo
  deleted_at timestamptz
);

CREATE TABLE audit_logs (
  id bigserial PRIMARY KEY, actor text NOT NULL, source text NOT NULL, action text NOT NULL,
  server_name text, request_id text, payload jsonb, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE tokens ( id text PRIMARY KEY, name text NOT NULL, token_hash text NOT NULL, scope jsonb NOT NULL, expires_at timestamptz );
