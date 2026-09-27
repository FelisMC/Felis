#!/bin/bash
# Seeds the database of the release the e2e upgrade job installed, and checks after the
# upgrade that every seeded row came through unchanged. The job runs it around the upgrade:
#
#   sudo bash deploy/e2e_seed.sh seed    # after the release installed
#   sudo bash deploy/e2e_seed.sh check   # after this commit ran over it
#
# A fresh install's database holds only the rows its migrations write, so without the seed
# the upgrade moves and migrates an almost empty database: the move's row-count comparison
# compares zeros and the pending migrations never meet an existing row. The seed writes the
# columns the oldest release has (v0.1.0), so it applies to every release since, and the
# check reads the same columns back: a migration that rewrites one of them on purpose
# updates the snapshot query here.
#
# Every row is inert. Nothing in it is a credential anyone holds: the session and the
# challenge carry the hash of bytes nobody kept and are spent already. Nothing is due for
# the reaper or the operator, and the retention sweep keeps spent rows for 30 days after
# they were spent. servers stays empty: its rows mirror MinecraftServer objects the
# operator and the reaper reconcile, and a row without one would not stay as written.
set -euo pipefail

phase="${1:?usage: e2e_seed.sh seed|check}"
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
KUBECTL=(/usr/local/bin/k3s kubectl)
STATE_DIR=/var/tmp/felis-e2e-seed
PG_MOVED_MARKER=/var/lib/felis/postgres-moved
TABLES=(users account_links quotas sessions webauthn_challenges platform_settings audit_logs
  world_backups image_builds image_submissions account_migrations op_login_requests)
fails=0

pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }

# db_where says where the platform's database lives: in felis-postgres once the install
# has it, else in the host PostgreSQL a release before it ran.
db_where() {
  if "${KUBECTL[@]}" -n felis get deploy/felis-postgres >/dev/null 2>&1; then
    echo pod
  else
    echo host
  fi
}

# db_psql runs the SQL on its stdin in the felis database at <where>.
db_psql() { # where
  case "$1" in
    pod)
      "${KUBECTL[@]}" -n felis exec -i deploy/felis-postgres -c postgres -- \
        psql -X -q -At -v ON_ERROR_STOP=1 -U felis -d felis
      ;;
    # From /, which the postgres user can always enter: psql warns about any other cwd.
    host) (cd / && runuser -u postgres -- psql -X -q -At -v ON_ERROR_STOP=1 -d felis) ;;
  esac
}

# snapshot prints every seeded row, one table after the other, each in key order. The host
# server's time zone is the host's and the pod's is UTC, so timestamps print in UTC.
snapshot() { # where
  db_psql "$1" <<'EOF'
SET TimeZone = 'UTC';
SELECT 'users ' || row(id, username, email, role, email_verified, disabled, deleted_at, created_at, updated_at)::text
  FROM users WHERE id LIKE 'e2e-seed-%' ORDER BY id;
SELECT 'account_links ' || row(user_id, mc_uuid, auth_source, verified_at)::text
  FROM account_links WHERE user_id LIKE 'e2e-seed-%' ORDER BY mc_uuid;
SELECT 'quotas ' || row(user_id, max_servers, max_cpu_milli, max_memory_mb, max_storage_gb, updated_by, updated_at)::text
  FROM quotas WHERE user_id LIKE 'e2e-seed-%' ORDER BY user_id;
SELECT 'sessions ' || row(token_hash, user_id, created_at, expires_at, revoked_at)::text
  FROM sessions WHERE user_id LIKE 'e2e-seed-%' ORDER BY token_hash;
SELECT 'webauthn_challenges ' || row(id, user_id, purpose, session_data, expires_at, consumed_at, created_at)::text
  FROM webauthn_challenges WHERE id LIKE 'e2e-seed-%' ORDER BY id;
SELECT 'platform_settings ' || row(key, value, updated_at)::text
  FROM platform_settings WHERE key = 'e2e_seed';
SELECT 'audit_logs ' || row(id, actor, source, action, server_name, request_id, payload, created_at)::text
  FROM audit_logs WHERE actor = 'e2e-seed-staff' ORDER BY id;
SELECT 'world_backups ' || row(id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at, deleted_at)::text
  FROM world_backups WHERE id LIKE 'e2e-seed-%' ORDER BY id;
SELECT 'image_builds ' || row(id, image_ref, status, dockerfile, context_ref, base_image, requested_by, job_name, log_ref, error, created_at, finished_at)::text
  FROM image_builds WHERE id LIKE 'e2e-seed-%' ORDER BY id;
SELECT 'image_submissions ' || row(id, submitted_by, display_name, context_ref, status, image_ref, build_id, reviewed_by, reject_reason, created_at, reviewed_at)::text
  FROM image_submissions WHERE id LIKE 'e2e-seed-%' ORDER BY id;
SELECT 'account_migrations ' || row(id, source_user_id, target_user_id, state, confirm_factor, confirmed_at, code_hash, code_expires_at, redeemed_at, created_at, updated_at)::text
  FROM account_migrations WHERE id LIKE 'e2e-seed-%' ORDER BY id;
SELECT 'op_login_requests ' || row(id, user_id, email, expires_at, created_at, consumed_at, approved_at, approved_by)::text
  FROM op_login_requests WHERE id LIKE 'e2e-seed-%' ORDER BY id;
EOF
}

# seed writes the rows in one transaction and keeps where they went and what they read as.
# Text carries quotes, backslashes, control characters and non-ASCII, and the bytea a NUL,
# so a dump or restore that mangles any of them shows in the check. Rows that the retention
# sweep would judge by age are dated now.
seed() {
  local where before t
  where="$(db_where)"
  if ! db_psql "$where" <<'EOF'; then
BEGIN;
INSERT INTO users (id, username, email, role, email_verified, disabled, created_at, updated_at) VALUES
  ('e2e-seed-player', 'e2e_seed_player', 'Seed.Player+e2e@example.com', 'user', true, false,
   '2026-01-02 03:04:05.678901+00', '2026-01-02 03:04:05.678901+00'),
  ('e2e-seed-staff', 'e2e_seed_staff', 'staff@example.com', 'admin', false, true,
   '2026-01-03 00:00:00+00', '2026-01-03 00:00:00+00');
INSERT INTO account_links (user_id, mc_uuid, auth_source, verified_at) VALUES
  ('e2e-seed-player', '5eed0000-e2e0-4000-8000-000000000001', 'thirdparty', '2026-01-02 04:00:00+00');
INSERT INTO quotas (user_id, max_servers, max_cpu_milli, max_memory_mb, max_storage_gb, updated_by, updated_at) VALUES
  ('e2e-seed-player', 2, 4000, 8192, NULL, 'e2e-seed-staff', '2026-01-04 00:00:00+00');
INSERT INTO sessions (token_hash, user_id, created_at, expires_at, revoked_at) VALUES
  (encode(sha256(convert_to(gen_random_uuid()::text, 'UTF8')), 'hex'), 'e2e-seed-player',
   now(), now() + interval '30 days', now());
INSERT INTO webauthn_challenges (id, user_id, purpose, session_data, expires_at, consumed_at, created_at) VALUES
  ('e2e-seed-challenge', 'e2e-seed-player', 'passkey_register', '\x00ff0a0d5c27'::bytea,
   now() + interval '5 minutes', now(), now());
INSERT INTO platform_settings (key, value, updated_at) VALUES
  ('e2e_seed', '{"text": "quote '' dq \" backslash \\ tab\t newline\n 猫 🐱", "n": 1.50, "list": [1, null, true, {"k": "v"}]}',
   '2026-01-05 00:00:00+00');
INSERT INTO audit_logs (actor, source, action, server_name, request_id, payload, created_at) VALUES
  ('e2e-seed-staff', 'panel', 'e2e.seed', NULL, 'e2e-seed-req-1', '{"reason": "种子 \"quoted\"", "ids": [1, 2, 3]}', now()),
  ('e2e-seed-staff', 'cli', 'e2e.seed', 'e2e-seed-world', 'e2e-seed-req-2', NULL, now());
INSERT INTO world_backups (id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at, deleted_at) VALUES
  ('e2e-seed-backup', 'e2e-seed-world', 'e2e-seed-player', 'e2e-seed/world.tar.zst', 123456789012, 'manual', 'deleted',
   '2026-01-06 00:00:00+00', '2026-04-06 00:00:00+00', '2026-04-07 00:00:00+00');
INSERT INTO image_builds (id, image_ref, status, dockerfile, context_ref, requested_by, error, created_at, finished_at) VALUES
  ('e2e-seed-build', 'registry.felis.svc:5000/user-uploads/e2e-seed:latest', 'failed',
   E'FROM scratch\nLABEL note="e2e seed"\n', 'e2e-seed/context', 'e2e-seed-staff', 'e2e seed: never built',
   '2026-01-07 00:00:00+00', '2026-01-07 00:01:00+00');
INSERT INTO image_submissions (id, submitted_by, display_name, context_ref, status, reviewed_by, reject_reason, created_at, reviewed_at) VALUES
  ('e2e-seed-submission', 'e2e-seed-player', '种子整合包 e2e', 'e2e-seed/submission', 'rejected', 'e2e-seed-staff', 'e2e seed',
   '2026-01-08 00:00:00+00', '2026-01-08 01:00:00+00');
INSERT INTO account_migrations (id, source_user_id, target_user_id, state, confirm_factor, confirmed_at, redeemed_at, created_at, updated_at) VALUES
  ('e2e-seed-migration', 'e2e-seed-staff', 'e2e-seed-player', 'redeemed', 'email_otp', '2026-01-09 00:00:00+00',
   '2026-01-09 00:05:00+00', '2026-01-09 00:00:00+00', '2026-01-09 00:05:00+00');
INSERT INTO op_login_requests (id, user_id, email, expires_at, created_at, consumed_at, approved_at, approved_by) VALUES
  ('e2e-seed-oplogin', 'e2e-seed-staff', 'staff@example.com', now() + interval '10 minutes', now(), now(), now(), 'e2e-seed-staff');
COMMIT;
EOF
    fail "seed the release's database (${where})"
    return
  fi
  mkdir -p "$STATE_DIR"
  printf '%s\n' "$where" > "${STATE_DIR}/where"
  if ! before="$(snapshot "$where")"; then
    fail "read the seeded rows back"
    return
  fi
  printf '%s\n' "$before" > "${STATE_DIR}/before"
  for t in "${TABLES[@]}"; do
    if grep -q "^${t} " <<<"$before"; then
      pass "seeded ${t} (${where})"
    else
      fail "seeded ${t} (${where}): the snapshot has no row of it"
    fi
  done
}

# check_upgrade reads the seeded rows from felis-postgres and compares them with what the
# release's database held. A database the upgrade moved off the host also left the marker.
check_upgrade() {
  local where after seq
  if [ ! -s "${STATE_DIR}/before" ]; then
    fail "the seed step left its snapshot in ${STATE_DIR}/before"
    return
  fi
  where="$(db_where)"
  if [ "$where" != pod ]; then
    fail "the database lives in felis-postgres after the upgrade"
    return
  fi
  if [ "$(cat "${STATE_DIR}/where")" = host ]; then
    if [ -f "$PG_MOVED_MARKER" ]; then
      pass "the upgrade moved the seeded host database into felis-postgres"
    else
      fail "the upgrade moved the seeded host database into felis-postgres: no ${PG_MOVED_MARKER}"
    fi
  fi
  if ! after="$(snapshot pod)"; then
    fail "read the seeded rows from felis-postgres"
    return
  fi
  if [ "$after" = "$(cat "${STATE_DIR}/before")" ]; then
    pass "every seeded row came through the upgrade unchanged"
  else
    fail "the seeded rows changed across the upgrade (< release, > felis-postgres):"
    diff "${STATE_DIR}/before" <(printf '%s\n' "$after") || true
  fi
  # A restore that loses a sequence's position hands out ids that are taken: the next
  # audit row would fail on its primary key.
  seq="$(db_psql pod <<<"SELECT coalesce(pg_sequence_last_value(pg_get_serial_sequence('audit_logs', 'id')::regclass), 0) >= (SELECT max(id) FROM audit_logs)")" || seq=error
  if [ "$seq" = t ]; then
    pass "audit_logs' id sequence is past every seeded id"
  else
    fail "audit_logs' id sequence is past every seeded id (${seq})"
  fi
}

case "$phase" in
  seed) seed ;;
  check) check_upgrade ;;
  *)
    echo "usage: e2e_seed.sh seed|check" >&2
    exit 2
    ;;
esac

if [ "$fails" -eq 0 ]; then
  echo "ALL PASS (seed ${phase})"
  exit 0
fi
echo "${fails} FAILED (seed ${phase})"
exit 1
