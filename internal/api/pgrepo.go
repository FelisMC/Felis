package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PGRepo is the production Repo backed by Postgres (spec §6). It owns only the
// business projection the CRD cannot express. The SQL here is exercised by
// integration tests against a live database, not the hermetic api_test.go suite.
type PGRepo struct {
	db *sql.DB
}

// NewPGRepo wraps an existing pool (from store.PostgresDriver.DB()).
func NewPGRepo(db *sql.DB) *PGRepo { return &PGRepo{db: db} }

func (p *PGRepo) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }

func (p *PGRepo) ServerBySubdomain(ctx context.Context, subdomain string) (*ServerRecord, error) {
	const q = `SELECT s.name, sa.subdomain, COALESCE(s.owner_id, ''), COALESCE(s.cached_phase, '')
		FROM server_aliases sa JOIN servers s ON s.name = sa.server_name
		WHERE sa.subdomain = $1 AND s.deleted_at IS NULL`
	var r ServerRecord
	switch err := p.db.QueryRowContext(ctx, q, subdomain).Scan(&r.Name, &r.Subdomain, &r.OwnerID, &r.CachedPhase); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &r, nil
}

func (p *PGRepo) ServerByName(ctx context.Context, name string) (*ServerRecord, error) {
	const q = `SELECT s.name, COALESCE(sa.subdomain, ''), COALESCE(s.owner_id, ''), COALESCE(s.cached_phase, '')
		FROM servers s LEFT JOIN server_aliases sa ON sa.server_name = s.name
		WHERE s.name = $1 AND s.deleted_at IS NULL`
	var r ServerRecord
	switch err := p.db.QueryRowContext(ctx, q, name).Scan(&r.Name, &r.Subdomain, &r.OwnerID, &r.CachedPhase); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &r, nil
}

func (p *PGRepo) IsLinked(ctx context.Context, userID string) (bool, error) {
	var ok bool
	err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM account_links WHERE user_id = $1)`, userID).Scan(&ok)
	return ok, err
}

// CreateLinkCode persists a one-time link code for a verified in-game UUID (spec
// §10). expires_at is supplied by the caller (the API clock + TTL) so expiry is
// driven by one authoritative clock. The code is a PRIMARY KEY; a collision on
// the crypto/rand value is astronomically unlikely but surfaces as a plain
// driver error (the caller can retry) rather than being masked here.
func (p *PGRepo) CreateLinkCode(ctx context.Context, code, mcUUID, authSource string, expiresAt time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO account_link_codes (code, mc_uuid, auth_source, expires_at) VALUES ($1, $2, $3, $4)`,
		code, mcUUID, authSource, expiresAt)
	return err
}

// VerifyLinkCode consumes a code for userID and writes the account_links binding
// in one transaction (spec §10). The different-user conflict is detected by a
// guarded SELECT inside the tx rather than by inspecting a driver-specific unique
// violation, so the logic is portable. On the conflict path the tx rolls back, so
// the code is NOT consumed — a wrong user must not be able to burn the real
// owner's pending code. The UNIQUE(mc_uuid) constraint is the last-resort guard
// against a concurrent racer that passed the SELECT; that loses to a 500, which
// is acceptable for this integration-only path.
func (p *PGRepo) VerifyLinkCode(ctx context.Context, userID, code string, now time.Time) (string, string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var mcUUID, authSource string
	switch err := tx.QueryRowContext(ctx,
		`SELECT mc_uuid, auth_source FROM account_link_codes
		 WHERE code = $1 AND expires_at > $2
		 FOR UPDATE`,
		code, now).Scan(&mcUUID, &authSource); {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", ErrLinkCodeInvalid
	case err != nil:
		return "", "", err
	}

	// If this UUID is already linked, only the same user may re-verify (idempotent);
	// a different user is a conflict and must not consume the code.
	var existingUser string
	switch err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM account_links WHERE mc_uuid = $1`, mcUUID).Scan(&existingUser); {
	case errors.Is(err, sql.ErrNoRows):
		// not yet linked — fall through to insert
	case err != nil:
		return "", "", err
	default:
		if existingUser != userID {
			return "", "", ErrConflict
		}
	}

	// Copy the code's auth_source onto the durable link. On the idempotent
	// re-verify path DO UPDATE refreshes it (a player who re-linked via a different
	// Yggdrasil this time gets the latest source stored), keeping the persisted
	// value equal to the one returned to the caller.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_links (user_id, mc_uuid, auth_source) VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, mc_uuid) DO UPDATE SET auth_source = EXCLUDED.auth_source`,
		userID, mcUUID, authSource); err != nil {
		return "", "", fmt.Errorf("write account link: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_link_codes WHERE code = $1`, code); err != nil {
		return "", "", fmt.Errorf("consume link code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return mcUUID, authSource, nil
}

// RedeemPlayerBindCode redeems a Bind Code into a player account + link in one
// transaction (console-tier access model). It mirrors VerifyLinkCode's structure —
// strict expiry against the passed clock, the durable-link write, and the DELETE
// that consumes the code — but creates or fetches the user instead of requiring one.
// See the Repo interface for the full contract. Like VerifyLinkCode a concurrent
// racer that passed the SELECT loses to the UNIQUE(mc_uuid)/UNIQUE(username) guard
// (a 500), acceptable for this integration-only path; the primary idempotency is the
// mc_uuid-keyed fetch below.
func (p *PGRepo) RedeemPlayerBindCode(ctx context.Context, newUserID, code string, now time.Time) (string, string, string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var mcUUID, authSource string
	switch err := tx.QueryRowContext(ctx,
		`SELECT mc_uuid, auth_source FROM account_link_codes WHERE code = $1 AND expires_at > $2`,
		code, now).Scan(&mcUUID, &authSource); {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", "", ErrLinkCodeInvalid
	case err != nil:
		return "", "", "", err
	}

	// Create-or-fetch keyed on the verified UUID. An already-linked role='user' player
	// is fetched (idempotent "log in via the game"); a role='admin' STAFF account is
	// refused (op.console only) BEFORE any consume, so the code survives; an unlinked
	// UUID births a fresh role='user' player with a uuid-derived unique username.
	userID := newUserID
	var existingRole string
	switch err := tx.QueryRowContext(ctx,
		`SELECT u.id, u.role::text FROM account_links al JOIN users u ON u.id = al.user_id WHERE al.mc_uuid = $1`,
		mcUUID).Scan(&userID, &existingRole); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, username, role) VALUES ($1, $2, 'user')`,
			newUserID, mcUUID); err != nil {
			return "", "", "", fmt.Errorf("create player: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO account_links (user_id, mc_uuid, auth_source) VALUES ($1, $2, $3)`,
			newUserID, mcUUID, authSource); err != nil {
			return "", "", "", fmt.Errorf("write account link: %w", err)
		}
		userID = newUserID
	case err != nil:
		return "", "", "", err
	default:
		if existingRole != "user" {
			return "", "", "", ErrPlayerBindForbidden // staff must use op.console
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_link_codes WHERE code = $1`, code); err != nil {
		return "", "", "", fmt.Errorf("consume link code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", "", err
	}
	return userID, mcUUID, authSource, nil
}

// CompleteOwnerSetup consumes an in-game link code, creates-or-promotes the bound
// account to the passwordless Owner (role='admin'), enables local auth, and stores
// the one-time first-login token in one transaction. It is the `felis setup`
// MC-bind path: the operator enters limbo, runs /link, and types the code here.
// Unlike RedeemPlayerBindCode — which refuses an already-staff account so a game
// login can never self-elevate — this DELIBERATELY elevates: an unlinked UUID is
// born directly as staff, and an already-linked account (player OR staff) is
// promoted in place, preserving its id so any live sessions and its username
// survive. The elevation is gated by the caller's local-root break-glass
// authority, not by anything in-band. Returns the Owner's (userID, mcUUID,
// authSource); an absent or expired code is ErrLinkCodeInvalid and consumes
// nothing. Any failure in the auth-toggle or token writes rolls the elevation and
// code consumption back, leaving the operator able to retry setup.
func (p *PGRepo) CompleteOwnerSetup(ctx context.Context, newUserID, code string, now time.Time,
	tokenHash string, tokenExpiresAt time.Time) (string, string, string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var mcUUID, authSource string
	switch err := tx.QueryRowContext(ctx,
		`SELECT mc_uuid, auth_source FROM account_link_codes WHERE code = $1 AND expires_at > $2`,
		code, now).Scan(&mcUUID, &authSource); {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", "", ErrLinkCodeInvalid
	case err != nil:
		return "", "", "", err
	}

	// Create-or-promote keyed on the verified UUID. An unlinked UUID births a fresh
	// staff row (role='admin') with a uuid-derived username; an already-linked
	// account is promoted to role='admin' in place (idempotent when it is already
	// staff), keeping its id and username. Setup elevates on purpose, so there is no
	// staff refusal here — that guard belongs to the player path only.
	userID := newUserID
	switch err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM account_links WHERE mc_uuid = $1`, mcUUID).Scan(&userID); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, username, role) VALUES ($1, $2, 'admin')`,
			newUserID, mcUUID); err != nil {
			return "", "", "", fmt.Errorf("create owner: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO account_links (user_id, mc_uuid, auth_source) VALUES ($1, $2, $3)`,
			newUserID, mcUUID, authSource); err != nil {
			return "", "", "", fmt.Errorf("write account link: %w", err)
		}
		userID = newUserID
	case err != nil:
		return "", "", "", err
	default:
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET role = 'admin' WHERE id = $1`, userID); err != nil {
			return "", "", "", fmt.Errorf("promote owner: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1, $2::jsonb)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		LocalAuthEnabledKey, "true"); err != nil {
		return "", "", "", fmt.Errorf("enable local auth: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO setup_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, tokenExpiresAt); err != nil {
		return "", "", "", fmt.Errorf("mint setup token: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_link_codes WHERE code = $1`, code); err != nil {
		return "", "", "", fmt.Errorf("consume link code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", "", err
	}
	return userID, mcUUID, authSource, nil
}

// QuotaAvailable treats a missing quota row or a NULL max_servers as unlimited;
// otherwise it compares the live owned-server count against the cap (spec §9.3).
//
// KNOWN-LIMITATION (audit #4, quota TOCTOU): this check and ClaimServer are two
// separate statements, not one transaction, so the count read here is not serialized
// against a concurrent claim's UPDATE. Two claims by the same user for two DIFFERENT
// ownerless servers can both read count < max_servers (under READ COMMITTED neither
// sees the other's uncommitted UPDATE) and both succeed, leaving the user one server
// over quota. Severity is low: it over-provisions the quota by a small margin under a
// deliberate concurrent burst — it is NOT an authorization, ownership, or isolation
// break (each server is still claimed atomically via UPDATE ... WHERE owner_id IS
// NULL, so two users never share one server). Closing it needs Postgres transaction
// semantics: wrap the count and a conditional UPDATE (gated on count < max_servers) in
// one tx under pg_advisory_xact_lock(hashtext(user_id)) — or SERIALIZABLE with a retry
// loop — folding the gate out of the two handlers (handleClaim and the internal UUID
// claim) into a single repo method. That is INTEGRATION-dependent: it is verifiable
// only against a real Postgres, not the hermetic fakeRepo suite, so it is documented
// here rather than patched blind.
func (p *PGRepo) QuotaAvailable(ctx context.Context, userID string) (bool, error) {
	var maxServers sql.NullInt64
	switch err := p.db.QueryRowContext(ctx,
		`SELECT max_servers FROM quotas WHERE user_id = $1`, userID).Scan(&maxServers); {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	case err != nil:
		return false, err
	}
	if !maxServers.Valid {
		return true, nil
	}
	var n int64
	if err := p.db.QueryRowContext(ctx,
		`SELECT count(*) FROM servers WHERE owner_id = $1 AND deleted_at IS NULL`, userID).Scan(&n); err != nil {
		return false, err
	}
	return n < maxServers.Int64, nil
}

// QuotaCheck reports whether accepting a server with resource spec `incoming`
// would push userID over any quota cap. excludeName is the server row whose own
// cached resources should be excluded ("" for a fresh claim where the row
// doesn't exist yet). Four dimensions are checked: server count, CPU millicores,
// memory MB, and storage MB. A NULL or missing quota row/column means unlimited
// for that dimension. Like QuotaAvailable, the count check and the write are not
// serialized — see the QuotaAvailable TOCTOU docstring.
func (p *PGRepo) QuotaCheck(ctx context.Context, userID string, excludeName string, incoming ResourceSpec) (bool, error) {
	var maxServers, maxCPU, maxMem, maxStor sql.NullInt64
	switch err := p.db.QueryRowContext(ctx,
		`SELECT max_servers, max_cpu_milli, max_memory_mb, max_storage_gb
		 FROM quotas WHERE user_id = $1`, userID).Scan(
		&maxServers, &maxCPU, &maxMem, &maxStor); {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil // no quota row → unlimited
	case err != nil:
		return false, err
	}

	var count int64
	var cpuSum, memSum, storSum sql.NullInt64
	switch err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cached_cpu_milli), 0), COALESCE(SUM(cached_memory_mb), 0), COALESCE(SUM(cached_storage_mb), 0)
		 FROM servers WHERE owner_id = $1 AND deleted_at IS NULL AND name != $2`,
		userID, excludeName).Scan(&count, &cpuSum, &memSum, &storSum); {
	case err != nil:
		return false, err
	}

	if maxServers.Valid && count >= maxServers.Int64 {
		return false, nil
	}
	if maxCPU.Valid && cpuSum.Int64+int64(incoming.CPUMilli) > maxCPU.Int64 {
		return false, nil
	}
	if maxMem.Valid && memSum.Int64+int64(incoming.MemoryMB) > maxMem.Int64 {
		return false, nil
	}
	if maxStor.Valid && storSum.Int64+int64(incoming.StorageMB) > maxStor.Int64*1024 {
		return false, nil
	}
	return true, nil
}

// UpdateServerResources updates the resource cache for a server after a spec
// mutation (spec §7 PATCH). The per-owner aggregate used by QuotaCheck is a
// SQL SUM over the cached columns, so every mutation must write through here.
func (p *PGRepo) UpdateServerResources(ctx context.Context, name string, cpuMilli, memoryMB, storageMB int) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE servers SET cached_cpu_milli = $2, cached_memory_mb = $3, cached_storage_mb = $4 WHERE name = $1 AND deleted_at IS NULL`,
		name, cpuMilli, memoryMB, storageMB)
	return err
}

// ServerResources returns the cached resource spec for a server.
func (p *PGRepo) ServerResources(ctx context.Context, name string) (ResourceSpec, error) {
	var r ResourceSpec
	switch err := p.db.QueryRowContext(ctx,
		`SELECT cached_cpu_milli, cached_memory_mb, cached_storage_mb FROM servers WHERE name = $1 AND deleted_at IS NULL`,
		name).Scan(&r.CPUMilli, &r.MemoryMB, &r.StorageMB); {
	case errors.Is(err, sql.ErrNoRows):
		return r, nil
	case err != nil:
		return r, err
	}
	return r, nil
}

// ClaimServer performs the atomic ownership transfer (spec §9.3). A missing
// server is ErrNotFound; an existing-but-owned server yields claimed=false so the
// handler can answer 409.
func (p *PGRepo) ClaimServer(ctx context.Context, name, userID string) (bool, error) {
	var exists bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM servers WHERE name = $1 AND deleted_at IS NULL)`, name).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, ErrNotFound
	}
	res, err := p.db.ExecContext(ctx,
		`UPDATE servers SET owner_id = $2, claimed_at = now() WHERE name = $1 AND owner_id IS NULL AND deleted_at IS NULL`,
		name, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (p *PGRepo) UserInAllowlist(ctx context.Context, name, userID string) (bool, error) {
	const q = `SELECT EXISTS(
		SELECT 1 FROM server_allowlist sa JOIN account_links al ON al.mc_uuid = sa.mc_uuid
		WHERE sa.server_name = $1 AND al.user_id = $2)`
	var ok bool
	err := p.db.QueryRowContext(ctx, q, name, userID).Scan(&ok)
	return ok, err
}

// UUIDInAllowlist is the internal-face allowlist check keyed by the in-game UUID
// directly (spec §9.4). The server_allowlist table is UUID-keyed, so the
// velocity-driven wake — which knows the joining player only by their online-mode
// UUID — needs no account_links bridge (contrast UserInAllowlist).
func (p *PGRepo) UUIDInAllowlist(ctx context.Context, name, mcUUID string) (bool, error) {
	const q = `SELECT EXISTS(
		SELECT 1 FROM server_allowlist WHERE server_name = $1 AND mc_uuid = $2)`
	var ok bool
	err := p.db.QueryRowContext(ctx, q, name, mcUUID).Scan(&ok)
	return ok, err
}

// UserByMCUUID resolves a verified in-game UUID to its linked user_id (spec §10
// account_links), or ErrNotFound when the UUID is not linked to any account.
func (p *PGRepo) UserByMCUUID(ctx context.Context, mcUUID string) (string, error) {
	var userID string
	switch err := p.db.QueryRowContext(ctx,
		`SELECT user_id FROM account_links WHERE mc_uuid = $1`, mcUUID).Scan(&userID); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", err
	}
	return userID, nil
}

// RecordJoin renews activity and auto-appends the UUID to the allowlist in one
// transaction (spec §7, §9.4). A missing server is ErrNotFound.
func (p *PGRepo) RecordJoin(ctx context.Context, name, mcUUID string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	res, err := tx.ExecContext(ctx,
		`UPDATE servers SET last_active_at = now(), warned_3d_at = NULL, warned_1d_at = NULL
		 WHERE name = $1 AND deleted_at IS NULL`, name)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_allowlist (server_name, mc_uuid) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		name, mcUUID); err != nil {
		return fmt.Errorf("append allowlist: %w", err)
	}
	return tx.Commit()
}

func (p *PGRepo) MyServers(ctx context.Context, userID string) ([]MyServerView, error) {
	const q = `SELECT s.name, COALESCE(sa.subdomain, ''),
		COALESCE(s.owner_id = $1, false) AS owned, (s.owner_id IS NULL) AS claimable, COALESCE(s.cached_phase, '')
		FROM servers s LEFT JOIN server_aliases sa ON sa.server_name = s.name
		WHERE s.deleted_at IS NULL AND (s.owner_id = $1 OR s.owner_id IS NULL)
		ORDER BY s.name`
	rows, err := p.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MyServerView
	for rows.Next() {
		var v MyServerView
		if err := rows.Scan(&v.Name, &v.Subdomain, &v.Owned, &v.Claimable, &v.Phase); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ServerOwners returns name -> owner display identity for every currently-owned,
// non-deleted server (the SysAdmin cockpit's fleet read). The INNER JOIN drops
// unclaimed servers (owner_id NULL) and the deleted_at filter drops soft-deleted
// ones, so the map holds only servers that have a live owner — the cockpit reads a
// missing key as "no owner". The display value prefers the recognizable email
// (the same identity the audit log records as the human actor, §6) and falls back
// to the never-NULL username when the address is absent.
func (p *PGRepo) ServerOwners(ctx context.Context) (map[string]string, error) {
	const q = `SELECT s.name, COALESCE(NULLIF(u.email, ''), u.username)
		FROM servers s JOIN users u ON u.id = s.owner_id
		WHERE s.deleted_at IS NULL`
	rows, err := p.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, owner string
		if err := rows.Scan(&name, &owner); err != nil {
			return nil, err
		}
		out[name] = owner
	}
	return out, rows.Err()
}

// SeedServer inserts the business rows backing a newly created server (spec
// §15): the servers row (owner_id left NULL — the server is created unowned and
// claimed later, spec §9.3) and its subdomain alias. Both inserts are
// ON CONFLICT DO NOTHING so a retried create is idempotent. The alias subdomain
// is a PRIMARY KEY, so a no-op insert means it was already bound; we then
// confirm it resolves to this server and return ErrConflict otherwise, letting
// the create handler answer 409 before it touches the CRD.
func (p *PGRepo) SeedServer(ctx context.Context, name, subdomain string, cpuMilli, memoryMB, storageMB int) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, $2, $3, $4) ON CONFLICT (name) DO UPDATE SET cached_cpu_milli = EXCLUDED.cached_cpu_milli, cached_memory_mb = EXCLUDED.cached_memory_mb, cached_storage_mb = EXCLUDED.cached_storage_mb`, name, cpuMilli, memoryMB, storageMB); err != nil {
		return fmt.Errorf("seed server row: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_aliases (subdomain, server_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		subdomain, name); err != nil {
		return fmt.Errorf("seed subdomain alias: %w", err)
	}
	var boundTo string
	if err := tx.QueryRowContext(ctx,
		`SELECT server_name FROM server_aliases WHERE subdomain = $1`, subdomain).Scan(&boundTo); err != nil {
		return fmt.Errorf("confirm subdomain alias: %w", err)
	}
	if boundTo != name {
		return ErrConflict
	}
	return tx.Commit()
}

// AllBackups lists every present world backup, newest first (spec §7 GET
// /backups, admin scope; world_backups in §22). Only status='present' rows are
// listed — an expired or deleted backup is gone (spec §466).
func (p *PGRepo) AllBackups(ctx context.Context) ([]BackupView, error) {
	const q = `SELECT id, server_name, COALESCE(former_owner, ''), COALESCE(size_bytes, 0),
		reason, status, created_at, expires_at
		FROM world_backups WHERE status = 'present' ORDER BY created_at DESC`
	rows, err := p.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return scanBackupViews(rows)
}

// BackupsForUser lists the present world backups of worlds the user formerly
// owned, newest first (spec §7 GET /backups, former_owner scope). A NULL
// former_owner never matches a user id, so orphaned backups stay admin-only.
func (p *PGRepo) BackupsForUser(ctx context.Context, userID string) ([]BackupView, error) {
	const q = `SELECT id, server_name, COALESCE(former_owner, ''), COALESCE(size_bytes, 0),
		reason, status, created_at, expires_at
		FROM world_backups WHERE status = 'present' AND former_owner = $1 ORDER BY created_at DESC`
	rows, err := p.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	return scanBackupViews(rows)
}

// scanBackupViews drains a world_backups result set into BackupViews. backup_ref
// is intentionally not selected — it never leaves the server (spec §286 principle).
func scanBackupViews(rows *sql.Rows) ([]BackupView, error) {
	defer rows.Close()
	var out []BackupView
	for rows.Next() {
		var v BackupView
		if err := rows.Scan(&v.ID, &v.ServerName, &v.FormerOwner, &v.SizeBytes,
			&v.Reason, &v.Status, &v.CreatedAt, &v.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// LatestBackup returns the most recent present backup for a server (spec §466
// restore), or ErrNotFound. Unlike the list queries this selects backup_ref — the
// caller (the restore handler) hands it to the Restorer and never serializes it.
func (p *PGRepo) LatestBackup(ctx context.Context, serverName string) (*BackupRecord, error) {
	const q = `SELECT id, server_name, COALESCE(former_owner, ''), backup_ref, COALESCE(size_bytes, 0)
		FROM world_backups WHERE server_name = $1 AND status = 'present'
		ORDER BY created_at DESC LIMIT 1`
	var b BackupRecord
	switch err := p.db.QueryRowContext(ctx, q, serverName).Scan(
		&b.ID, &b.ServerName, &b.FormerOwner, &b.BackupRef, &b.SizeBytes); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &b, nil
}

// BackupByID returns a single present backup by its id, or ErrNotFound.
func (p *PGRepo) BackupByID(ctx context.Context, id string) (*BackupRecord, error) {
	const q = `SELECT id, server_name, COALESCE(former_owner, ''), backup_ref, COALESCE(size_bytes, 0)
		FROM world_backups WHERE id = $1 AND status = 'present'`
	var b BackupRecord
	switch err := p.db.QueryRowContext(ctx, q, id).Scan(
		&b.ID, &b.ServerName, &b.FormerOwner, &b.BackupRef, &b.SizeBytes); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &b, nil
}

func (p *PGRepo) Audit(ctx context.Context, e AuditEntry) error {
	// A nil Payload must land as SQL NULL, not the text "null"; a non-nil Payload is
	// passed as a JSON text the jsonb column parses (same idiom as reaper.PGStore).
	var payload any
	if len(e.Payload) > 0 {
		payload = string(e.Payload)
	}
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO audit_logs (actor, source, action, server_name, request_id, payload)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6)`,
		e.Actor, e.Source, e.Action, e.ServerName, e.RequestID, payload)
	return err
}

// ---- player email verification (spec §B2 onboarding) ----

// CreateEmailOTP supersedes any prior live code for (user, purpose) and inserts the
// fresh one, in one transaction (spec §B2). The supersede DELETE means a re-request
// invalidates the earlier mail, so only the most recent code can ever verify — a
// player who requested twice cannot be confused into typing the stale digits. Only
// the hash is stored; the digits live only in the email.
func (p *PGRepo) CreateEmailOTP(ctx context.Context, id, userID, email, codeHash, purpose string, expiresAt time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM email_otps WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL`,
		userID, purpose); err != nil {
		return fmt.Errorf("supersede prior otp: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO email_otps (id, user_id, email, code_hash, purpose, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, userID, email, codeHash, purpose, expiresAt); err != nil {
		return fmt.Errorf("insert otp: %w", err)
	}
	return tx.Commit()
}

// VerifyEmailOTP redeems the newest live code for (user, purpose) in one
// transaction (spec §B2). The row is taken FOR UPDATE so a concurrent verify of the
// same code cannot double-spend it. The branch order is deliberate: expiry and the
// attempt cap are checked before the hash compare, so an expired or locked code is
// never silently accepted, and a hash mismatch costs an attempt (UPDATE attempts+1)
// without consuming the code — a typo must not burn a still-valid code. On a match
// the code is consumed and the user row is flipped verified, returning the proven
// address. ErrOTPInvalid / ErrOTPLocked are the only domain errors.
func (p *PGRepo) VerifyEmailOTP(ctx context.Context, userID, purpose, codeHash string, now time.Time) (string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var (
		id         string
		email      string
		storedHash string
		attempts   int
		expiresAt  time.Time
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT id, email, code_hash, attempts, expires_at FROM email_otps
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
		 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		userID, purpose).Scan(&id, &email, &storedHash, &attempts, &expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrOTPInvalid
	case err != nil:
		return "", err
	}

	if !expiresAt.After(now) {
		return "", ErrOTPInvalid
	}
	if attempts >= otpMaxAttempts {
		return "", ErrOTPLocked
	}
	if storedHash != codeHash {
		if _, err := tx.ExecContext(ctx,
			`UPDATE email_otps SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
			return "", fmt.Errorf("record otp attempt: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return "", ErrOTPInvalid
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE email_otps SET consumed_at = $2 WHERE id = $1`, id, now); err != nil {
		return "", fmt.Errorf("consume otp: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET email = $2, email_verified = true WHERE id = $1`, userID, email); err != nil {
		return "", fmt.Errorf("mark email verified: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return email, nil
}

// SetUserEmail records email on the user row WITHOUT verifying it (setup bootstrap
// has no SMTP — the Owner enters an address a later Settings/SMTP flow will verify).
// It clears email_verified in the same write: only VerifyEmailOTP ever sets that
// flag, and it does so only alongside the proven address, so recording a fresh
// (unproven) address must drop any prior verification rather than leave a stale
// email_verified=true asserting an address the user never proved. For a fresh Owner
// the flag is already false, so this is a no-op there.
func (p *PGRepo) SetUserEmail(ctx context.Context, userID, email string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE users SET email = $2, email_verified = false WHERE id = $1`, userID, email)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- player game-login: username-collision reclaim (spec §B3) ----

// ReclaimUsername bars the squatter UUID and stashes its data hold in one
// transaction (spec §B3 正版优先), returning the hold's EFFECTIVE expiry — the
// value actually persisted, which the caller echoes so the rejected player is
// told the truth about how long their data is kept. The blacklist insert is
// ON CONFLICT (mc_uuid) DO NOTHING; the hold insert is a no-op DO UPDATE so a
// retried reclaim of an already-stashed UUID does not move the original window
// yet RETURNING still fires, handing back the FIRST reclaim's expires_at rather
// than a fresh now()+TTL (DO NOTHING would suppress RETURNING and lose it). The
// two writes share the tx so a failure on the second rolls back the first: the
// system is never left with a barred UUID whose data was never held (data loss)
// nor a hold for a UUID still able to connect (squatter not barred). dataRef ""
// lands as SQL NULL (the column is nullable — archival may be deferred),
// mirroring the NULLIF idiom used for optional text elsewhere.
func (p *PGRepo) ReclaimUsername(ctx context.Context, id, squatterUUID, username, dataRef string, expiresAt time.Time) (time.Time, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO username_blacklist (mc_uuid, username) VALUES ($1, $2)
		 ON CONFLICT (mc_uuid) DO NOTHING`,
		squatterUUID, username); err != nil {
		return time.Time{}, fmt.Errorf("blacklist squatter uuid: %w", err)
	}
	var effective time.Time
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO player_data_holds (id, mc_uuid, username, data_ref, expires_at)
		 VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		 ON CONFLICT (mc_uuid) DO UPDATE SET mc_uuid = EXCLUDED.mc_uuid
		 RETURNING expires_at`,
		id, squatterUUID, username, dataRef, expiresAt).Scan(&effective); err != nil {
		return time.Time{}, fmt.Errorf("stash data hold: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return effective, nil
}

// IsUsernameBlacklisted reports whether an in-game UUID is barred by a prior
// reclaim (spec §B3). It is a single EXISTS keyed by the UUID — the genuine
// Mojang player, who shares the contested name under a different UUID, never
// matches.
func (p *PGRepo) IsUsernameBlacklisted(ctx context.Context, mcUUID string) (bool, error) {
	var ok bool
	err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM username_blacklist WHERE mc_uuid = $1)`, mcUUID).Scan(&ok)
	return ok, err
}

// IsProtectedAdminLink reports whether mc_uuid belongs to a Linked Operator/SysAdmin
// who authenticates through the third-party Yggdrasil — the admin-on-Yggdrasil reclaim
// exception (spec §B3). The EXISTS joins account_links to users on exactly three
// conjuncts: the UUID is linked, that link authenticated via 'thirdparty', and the
// linked user is an admin. It intentionally does not test HOW the account signs in:
// an Operator may authenticate via SSO (Cloudflare Access, §14) or any local
// passwordless door and must be protected just the same — the sign-in method is
// orthogonal to "is staff" and "logs in via the Login Server". Keyed by UUID, the
// only identity velocity holds.
func (p *PGRepo) IsProtectedAdminLink(ctx context.Context, mcUUID string) (bool, error) {
	var ok bool
	err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM account_links al JOIN users u ON u.id = al.user_id
			WHERE al.mc_uuid = $1 AND al.auth_source = 'thirdparty' AND u.role = 'admin')`,
		mcUUID).Scan(&ok)
	return ok, err
}

// ---- staff account lookups (spec §B, passwordless) ----

// UserByUsername loads a staff login projection by username, or ErrNotFound.
// The account is passwordless — staff authenticate via email-OTP / passkey, so
// no password column is read.
func (p *PGRepo) UserByUsername(ctx context.Context, username string) (*StaffUser, error) {
	const q = `SELECT id, username, COALESCE(email, ''), role::text, email_verified
		FROM users WHERE username = $1`
	var u StaffUser
	switch err := p.db.QueryRowContext(ctx, q, username).Scan(
		&u.ID, &u.Username, &u.Email, &u.Role, &u.EmailVerified); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &u, nil
}

// AdminExists reports whether any admin account already exists. It is the
// break-glass console's bootstrap-vs-recovery switch: false means the typed
// credential mints the first Owner (no prior identity to verify against), true
// means the operator must identify against an existing admin for accountability.
// It is not on the Repo interface because only the break-glass CLI consults it.
func (p *PGRepo) AdminExists(ctx context.Context) (bool, error) {
	const q = `SELECT 1 FROM users WHERE role = 'admin' LIMIT 1`
	var one int
	switch err := p.db.QueryRowContext(ctx, q).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// UserByID loads the same staff projection by id, or ErrNotFound. The
// account is passwordless — no password column is read.
func (p *PGRepo) UserByID(ctx context.Context, id string) (*StaffUser, error) {
	const q = `SELECT id, username, COALESCE(email, ''), role::text, email_verified
		FROM users WHERE id = $1`
	var u StaffUser
	switch err := p.db.QueryRowContext(ctx, q, id).Scan(
		&u.ID, &u.Username, &u.Email, &u.Role, &u.EmailVerified); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &u, nil
}

// UpsertOwner creates or resets the Owner account direct-to-Postgres (the
// break-glass first-run / recovery path). role is forced to 'admin' — the
// platform-level identity. On a username conflict the email is overwritten
// while the existing id is preserved, so live sessions referencing it survive
// a reset. The account is passwordless by design. The empty email is stored
// as NULL (users.email is nullable).
func (p *PGRepo) UpsertOwner(ctx context.Context, id, username, email string) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO users (id, username, email, role) VALUES ($1, $2, NULLIF($3, ''), 'admin')
		 ON CONFLICT (username) DO UPDATE SET email = EXCLUDED.email`,
		id, username, email)
	return err
}

// InsertOperator mints a NEW Operator (additional staff admin) account
// direct-to-Postgres. role is forced to 'admin'. UNLIKE UpsertOwner this is
// insert-only: a username conflict is left untouched and surfaces as a driver
// error, so adding an Operator can never silently reset the Owner's or another
// Operator's row. The account is passwordless by design. The empty email is
// stored as NULL.
func (p *PGRepo) InsertOperator(ctx context.Context, id, username, email string) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO users (id, username, email, role) VALUES ($1, $2, NULLIF($3, ''), 'admin')`,
		id, username, email)
	return err
}

// CreateSession records a minted session by the sha-256 of its cookie value
// (spec §B). Only the hash is stored, mirroring tokens.
func (p *PGRepo) CreateSession(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt)
	return err
}

// SessionUser resolves a live (unrevoked, unexpired at now) session hash to its
// user, or ErrNotFound.
func (p *PGRepo) SessionUser(ctx context.Context, tokenHash string, now time.Time) (*SessionedUser, error) {
	const q = `SELECT u.id, COALESCE(u.email, ''), u.role::text, COALESCE(u.email_verified, false)
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > $2`
	var u SessionedUser
	switch err := p.db.QueryRowContext(ctx, q, tokenHash, now).Scan(
		&u.ID, &u.Email, &u.Role, &u.EmailVerified); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &u, nil
}

// RevokeSession marks a session revoked (logout). Idempotent: a missing or
// already-revoked session is not an error.
func (p *PGRepo) RevokeSession(ctx context.Context, tokenHash string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`,
		tokenHash)
	return err
}

// ---- runtime platform settings (spec §B platform_settings) ----

// GetSetting reads a setting's raw jsonb value as bytes, or ErrNotFound.
func (p *PGRepo) GetSetting(ctx context.Context, key string) ([]byte, error) {
	var value []byte
	switch err := p.db.QueryRowContext(ctx,
		`SELECT value FROM platform_settings WHERE key = $1`, key).Scan(&value); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return value, nil
}

// SetSetting upserts a setting's raw jsonb value by key. value is cast to jsonb
// so a []byte argument lands in the jsonb column without a driver round-trip
// guessing the type.
func (p *PGRepo) SetSetting(ctx context.Context, key string, value []byte) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1, $2::jsonb)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, string(value))
	return err
}

// ---- player passkey enrollment (spec §14 WebAuthn / Phase 6 bind, migration 0007) ----

// CreatePasskeyChallenge supersedes any prior challenge for (user, purpose) and inserts
// the fresh one, in one transaction (mirrors CreateEmailOTP). The supersede DELETE
// removes ALL prior rows for (user, purpose) — not just the live one — so a re-begin
// invalidates the earlier ceremony AND reaps any already-consumed or expired row it left
// behind. That bounds the table at one row per (user, purpose): the begin→finish loop
// nets zero growth, since each begin sweeps the consumed row the previous finish stamped.
// (Deleting a consumed row is safe: it has already been redeemed and nothing reads it.)
// The opaque SessionData is held server-side so the client cannot forge the challenge
// it must answer at finish.
func (p *PGRepo) CreatePasskeyChallenge(ctx context.Context, id, userID, purpose string, sessionData []byte, expiresAt time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webauthn_challenges WHERE user_id = $1 AND purpose = $2`,
		userID, purpose); err != nil {
		return fmt.Errorf("supersede prior passkey challenge: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO webauthn_challenges (id, user_id, purpose, session_data, expires_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, userID, purpose, sessionData, expiresAt); err != nil {
		return fmt.Errorf("insert passkey challenge: %w", err)
	}
	return tx.Commit()
}

// ConsumePasskeyChallengeByUser redeems the newest live (unconsumed, unexpired at now)
// challenge for (user, purpose) in one transaction (mirrors VerifyEmailOTP). The row is
// taken FOR UPDATE so a concurrent finish cannot double-spend it; expiry is checked
// before consuming so a stale challenge is never accepted. On success consumed_at is
// stamped (single-use) and the stashed SessionData is returned. No live row →
// ErrPasskeyChallengeInvalid.
func (p *PGRepo) ConsumePasskeyChallengeByUser(ctx context.Context, userID, purpose string, now time.Time) ([]byte, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var (
		id          string
		sessionData []byte
		expiresAt   time.Time
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT id, session_data, expires_at FROM webauthn_challenges
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
		 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		userID, purpose).Scan(&id, &sessionData, &expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrPasskeyChallengeInvalid
	case err != nil:
		return nil, err
	}

	if !expiresAt.After(now) {
		return nil, ErrPasskeyChallengeInvalid
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE webauthn_challenges SET consumed_at = $2 WHERE id = $1`, id, now); err != nil {
		return nil, fmt.Errorf("consume passkey challenge: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sessionData, nil
}

// ---- discoverable ("usernameless") passkey login (task #40, migration 0013) ----

// maxLiveDiscoverableChallenges hard-bounds the non-user-keyed discoverable-login challenge
// store. webauthn_challenges self-bounds via a per-(user,purpose) supersede; a from-zero begin
// has no such key, so the table is capped: once this many LIVE (unexpired, unconsumed) rows
// exist, a new begin is refused (ErrTooManyDiscoverableChallenges → 429). The cap is generous —
// a login challenge lives only passkeyChallengeTTL (5 min) and each row is ~1 KB — so real
// concurrency never approaches it, while an abusive begin-flood is bounded to a few MB instead
// of growing without limit. Volumetric per-IP limiting is the edge's job (handlers_auth_email.go):
// behind Cloudflare RemoteAddr is the proxy, and a usernameless door has no recipient to key a
// fair per-caller limit on.
const maxLiveDiscoverableChallenges = 4096

// CreateDiscoverableChallenge stashes a discoverable-login ceremony under an opaque handle,
// bounding the table in one transaction (see the Repo interface for the full contract). It
// reaps expired/consumed rows first — the non-user-keyed analog of CreatePasskeyChallenge's
// supersede — then refuses over the cap rather than inserting. Because the reap ran first, the
// COUNT is exactly the live-row count, so the cap bounds an adversarial begin-flood (which a
// reap alone cannot: a burst inside the TTL leaves every fresh row live).
func (p *PGRepo) CreateDiscoverableChallenge(ctx context.Context, id string, sessionData []byte, now, expiresAt time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webauthn_discoverable_challenges WHERE expires_at <= $1 OR consumed_at IS NOT NULL`,
		now); err != nil {
		return fmt.Errorf("reap discoverable challenges: %w", err)
	}
	var live int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM webauthn_discoverable_challenges`).Scan(&live); err != nil {
		return fmt.Errorf("count discoverable challenges: %w", err)
	}
	if live >= maxLiveDiscoverableChallenges {
		return ErrTooManyDiscoverableChallenges
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO webauthn_discoverable_challenges (id, session_data, expires_at)
		 VALUES ($1, $2, $3)`,
		id, sessionData, expiresAt); err != nil {
		return fmt.Errorf("insert discoverable challenge: %w", err)
	}
	return tx.Commit()
}

// ConsumeDiscoverableChallenge redeems the challenge under handle id, single-use (see the Repo
// interface for the contract). The row is taken FOR UPDATE so a concurrent finish cannot
// double-spend it; expiry is checked before consuming. No live row → ErrPasskeyChallengeInvalid.
func (p *PGRepo) ConsumeDiscoverableChallenge(ctx context.Context, id string, now time.Time) ([]byte, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var (
		sessionData []byte
		expiresAt   time.Time
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT session_data, expires_at FROM webauthn_discoverable_challenges
		 WHERE id = $1 AND consumed_at IS NULL FOR UPDATE`,
		id).Scan(&sessionData, &expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrPasskeyChallengeInvalid
	case err != nil:
		return nil, err
	}

	if !expiresAt.After(now) {
		return nil, ErrPasskeyChallengeInvalid
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE webauthn_discoverable_challenges SET consumed_at = $2 WHERE id = $1`, id, now); err != nil {
		return nil, fmt.Errorf("consume discoverable challenge: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sessionData, nil
}

// CreatePasskeyCredential stores a freshly verified passkey (enrollment). Only public
// attestation material is written; a credential_id already bound to ANY account is left
// untouched (ON CONFLICT DO NOTHING) and reported as ErrConflict via a zero RowsAffected,
// so an authenticator is never silently rebound. Empty aaguid/name land as SQL NULL.
func (p *PGRepo) CreatePasskeyCredential(ctx context.Context, c PasskeyCredential) error {
	res, err := p.db.ExecContext(ctx,
		`INSERT INTO webauthn_credentials
		   (id, user_id, credential_id, public_key, sign_count, aaguid, name, created_at,
		    user_verified, backup_eligible, backup_state)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10, $11)
		 ON CONFLICT (credential_id) DO NOTHING`,
		c.ID, c.UserID, c.CredentialID, c.PublicKey, int64(c.SignCount), c.AAGUID, c.Name, c.CreatedAt,
		c.UserVerified, c.BackupEligible, c.BackupState)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// PasskeyCredentialsForUser lists the passkeys a user has bound, newest first, for the
// credential-management view. Nullable aaguid/name collapse to "" via COALESCE; the
// nullable last_used_at maps to a *time.Time (nil until an assertion is verified).
func (p *PGRepo) PasskeyCredentialsForUser(ctx context.Context, userID string) ([]PasskeyCredential, error) {
	const q = `SELECT id, user_id, credential_id, public_key, sign_count,
		COALESCE(aaguid, ''), COALESCE(name, ''), created_at, last_used_at,
		user_verified, backup_eligible, backup_state
		FROM webauthn_credentials WHERE user_id = $1 ORDER BY created_at DESC`
	rows, err := p.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PasskeyCredential
	for rows.Next() {
		var (
			c         PasskeyCredential
			signCount int64
			lastUsed  sql.NullTime
		)
		if err := rows.Scan(&c.ID, &c.UserID, &c.CredentialID, &c.PublicKey, &signCount,
			&c.AAGUID, &c.Name, &c.CreatedAt, &lastUsed,
			&c.UserVerified, &c.BackupEligible, &c.BackupState); err != nil {
			return nil, err
		}
		c.SignCount = uint32(signCount)
		if lastUsed.Valid {
			t := lastUsed.Time
			c.LastUsedAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AdvanceCredentialSignCount records a successful assertion on the passkey identified by
// credentialID: it advances the stored signature counter to newSignCount and stamps
// last_used_at. credential_id is UNIQUE so exactly one row is touched; a missing row (the
// credential was unbound mid-ceremony) affects zero rows and is a successful no-op, never an
// error — the assertion is already cryptographically complete by the time this runs.
func (p *PGRepo) AdvanceCredentialSignCount(ctx context.Context, credentialID string, newSignCount uint32, usedAt time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE webauthn_credentials SET sign_count = $2, last_used_at = $3 WHERE credential_id = $1`,
		credentialID, int64(newSignCount), usedAt)
	return err
}

// DeletePasskeyCredential removes the passkey row id, scoped to userID so a caller can
// only unbind their OWN credential. No matching (user, id) row → ErrNotFound via a zero
// RowsAffected, so a stale or cross-user id cannot silently no-op as success.
func (p *PGRepo) DeletePasskeyCredential(ctx context.Context, userID, id string) error {
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAllPasskeyCredentialsForUser unbinds every passkey a user holds. Unlike the
// single-credential delete this does NOT report ErrNotFound on zero rows: removing all of
// a user's passkeys when they have none is a successful no-op, since "the user holds no
// passkeys" is exactly the intended post-condition. It is the remediation that stops a
// passkey planted through a transiently-hijacked session from surviving; its production
// caller is the owner-tier DELETE /users/{id}/passkeys, which a complete remediation
// pairs with a session revoke (unbinding alone leaves the live hijacked session).
func (p *PGRepo) DeleteAllPasskeyCredentialsForUser(ctx context.Context, userID string) error {
	_, err := p.db.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE user_id = $1`, userID)
	return err
}

// ---- user admin (spec §7, admin-only) ----

// ListUsers returns a page of non-deleted users matching the optional filters,
// newest first. total is the unfiltered count so the admin page can render
// pagination without a second round-trip.
func (p *PGRepo) ListUsers(ctx context.Context, opts ListUsersOpts) ([]UserView, int, error) {
	var total int
	{
		q := `SELECT count(*) FROM users WHERE deleted_at IS NULL`
		if err := p.db.QueryRowContext(ctx, q).Scan(&total); err != nil {
			return nil, 0, err
		}
	}

	limit := opts.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

	// Build the WHERE clause from filters. All args are positional so the order
	// of appends must match.
	where := ` WHERE u.deleted_at IS NULL`
	var args []any
	argn := 0

	if opts.Query != "" {
		argn++
		where += fmt.Sprintf(` AND (u.username ILIKE '%%' || $%d || '%%' OR u.email ILIKE '%%' || $%d || '%%')`, argn, argn)
		args = append(args, opts.Query)
	}
	if opts.Role != "" {
		argn++
		where += fmt.Sprintf(` AND u.role::text = $%d`, argn)
		args = append(args, opts.Role)
	}
	switch opts.Hidden {
	case "true":
		where += ` AND u.disabled = true`
	case "false":
		where += ` AND u.disabled = false`
	}

	q := `SELECT u.id, u.username, COALESCE(u.email, ''), u.role::text,
		u.disabled, u.email_verified,
		u.created_at, u.updated_at,
		COALESCE((SELECT count(*) FROM servers s WHERE s.owner_id = u.id AND s.deleted_at IS NULL), 0)
	  FROM users u` + where
	argn++
	q += fmt.Sprintf(` ORDER BY u.created_at DESC LIMIT $%d OFFSET $%d`, argn, argn+1)
	args = append(args, limit, offset)

	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []UserView
	for rows.Next() {
		var v UserView
		if err := rows.Scan(&v.ID, &v.Username, &v.Email, &v.Role,
			&v.Disabled, &v.EmailVerified,
			&v.CreatedAt, &v.UpdatedAt, &v.ServerCount); err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

// UserDetail loads one user with its linked MC accounts, or ErrNotFound.
func (p *PGRepo) UserDetail(ctx context.Context, userID string) (*UserDetail, error) {
	const q = `SELECT u.id, u.username, COALESCE(u.email, ''), u.role::text,
		u.disabled, u.email_verified,
		u.created_at, u.updated_at, u.deleted_at,
		COALESCE((SELECT count(*) FROM servers s WHERE s.owner_id = u.id AND s.deleted_at IS NULL), 0)
	  FROM users u WHERE u.id = $1`
	var d UserDetail
	switch err := p.db.QueryRowContext(ctx, q, userID).Scan(
		&d.ID, &d.Username, &d.Email, &d.Role,
		&d.Disabled, &d.EmailVerified,
		&d.CreatedAt, &d.UpdatedAt, &d.DeletedAt, &d.ServerCount); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}

	// Load linked MC accounts.
	linkRows, err := p.db.QueryContext(ctx,
		`SELECT mc_uuid::text, COALESCE(auth_source, 'mojang'), verified_at
		 FROM account_links WHERE user_id = $1 ORDER BY verified_at`, userID)
	if err != nil {
		return &d, nil // best-effort; linked accounts are informational
	}
	defer linkRows.Close()
	for linkRows.Next() {
		var a LinkedAccount
		if err := linkRows.Scan(&a.MCUUID, &a.AuthSource, &a.VerifiedAt); err != nil {
			return &d, nil
		}
		d.LinkedAccounts = append(d.LinkedAccounts, a)
	}
	return &d, linkRows.Err()
}

// CreateUser mints a new user row. A username conflict → ErrConflict.
func (p *PGRepo) CreateUser(ctx context.Context, input CreateUserInput, _ string) (*UserView, error) {
	const q = `INSERT INTO users (id, username, email, role)
		VALUES (gen_random_uuid()::text, $1, NULLIF($2, ''), $3::user_role)
		ON CONFLICT (username) DO NOTHING
		RETURNING id, username, COALESCE(email, ''), role::text, disabled, email_verified,
			created_at, updated_at, 0`
	var v UserView
	switch err := p.db.QueryRowContext(ctx, q,
		input.Username, input.Email, input.Role).Scan(
		&v.ID, &v.Username, &v.Email, &v.Role,
		&v.Disabled, &v.EmailVerified,
		&v.CreatedAt, &v.UpdatedAt, &v.ServerCount); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrConflict
	case err != nil:
		return nil, err
	}
	return &v, nil
}

// UpdateUser applies the non-nil fields of patch and returns the updated view.
// A username conflict → ErrConflict; a non-existent user → ErrNotFound.
func (p *PGRepo) UpdateUser(ctx context.Context, userID string, patch UpdateUserInput, _ string) (*UserView, error) {
	// Build a dynamic SET clause from non-nil patch fields.
	var sets []string
	var args []any
	argn := 0
	if patch.Username != nil {
		argn++
		sets = append(sets, fmt.Sprintf("username = $%d", argn))
		args = append(args, *patch.Username)
	}
	if patch.Email != nil {
		argn++
		sets = append(sets, fmt.Sprintf("email = NULLIF($%d, '')", argn))
		args = append(args, *patch.Email)
	}
	if patch.Role != nil {
		argn++
		sets = append(sets, fmt.Sprintf("role = $%d::user_role", argn))
		args = append(args, *patch.Role)
	}
	if len(sets) == 0 {
		// No fields to update; return the current view.
		v, err := p.userView(ctx, userID)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	argn++
	args = append(args, userID)

	q := "UPDATE users SET " + sets[0]
	for _, s := range sets[1:] {
		q += ", " + s
	}
	q += fmt.Sprintf(` WHERE id = $%d AND deleted_at IS NULL`, argn)
	q += ` RETURNING id, username, COALESCE(email, ''), role::text, disabled,
		email_verified, created_at, updated_at,
		(SELECT count(*) FROM servers WHERE owner_id = users.id AND deleted_at IS NULL)`
	var v UserView
	switch err := p.db.QueryRowContext(ctx, q, args...).Scan(
		&v.ID, &v.Username, &v.Email, &v.Role,
		&v.Disabled, &v.EmailVerified,
		&v.CreatedAt, &v.UpdatedAt, &v.ServerCount); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		// A username UNIQUE violation surfaces as a driver error; map it to
		// ErrConflict so the handler can answer 409.
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return &v, nil
}

// userView returns a live user's projection, or ErrNotFound. It is the read half
// shared by UpdateUser (no-op return) and several other paths.
func (p *PGRepo) userView(ctx context.Context, userID string) (*UserView, error) {
	const q = `SELECT id, username, COALESCE(email, ''), role::text, disabled,
		email_verified, created_at, updated_at,
		(SELECT count(*) FROM servers WHERE owner_id = users.id AND deleted_at IS NULL)
		FROM users WHERE id = $1 AND deleted_at IS NULL`
	var v UserView
	switch err := p.db.QueryRowContext(ctx, q, userID).Scan(
		&v.ID, &v.Username, &v.Email, &v.Role,
		&v.Disabled, &v.EmailVerified,
		&v.CreatedAt, &v.UpdatedAt, &v.ServerCount); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &v, nil
}

// DeleteUser soft-deletes a user in one transaction: sets deleted_at, revokes
// every live session, and releases every owned server. The row is preserved so
// audit_logs.actor references survive.
func (p *PGRepo) DeleteUser(ctx context.Context, userID, _ string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Verify the user exists and is not already deleted.
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		userID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}

	// Release all owned servers.
	if _, err := tx.ExecContext(ctx,
		`UPDATE servers SET owner_id = NULL WHERE owner_id = $1 AND deleted_at IS NULL`,
		userID); err != nil {
		return err
	}

	// Revoke every live session.
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID); err != nil {
		return err
	}

	// Soft-delete the user row.
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET disabled = true, deleted_at = now() WHERE id = $1`,
		userID); err != nil {
		return err
	}

	return tx.Commit()
}

// SetUserDisabled flips the disabled flag. Setting disabled→true additionally
// revokes every live session so the account is immediately locked out.
func (p *PGRepo) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	// Guard: the user must exist and not be deleted.
	var ok bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		userID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}

	if _, err := p.db.ExecContext(ctx,
		`UPDATE users SET disabled = $2 WHERE id = $1`, userID, disabled); err != nil {
		return err
	}

	if disabled {
		// Revoke every live session so the lockout is immediate.
		_, _ = p.db.ExecContext(ctx,
			`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
			userID)
	}
	return nil
}

// ---- quota admin ----

// GetQuotas returns the quotas row for a user, or a zero-value view when no
// row exists (meaning unlimited).
func (p *PGRepo) GetQuotas(ctx context.Context, userID string) (*QuotaView, error) {
	const q = `SELECT user_id, max_servers, max_cpu_milli, max_memory_mb, max_storage_gb
		FROM quotas WHERE user_id = $1`
	v := QuotaView{UserID: userID}
	switch err := p.db.QueryRowContext(ctx, q, userID).Scan(
		&v.UserID, &v.MaxServers, &v.MaxCPUMilli, &v.MaxMemoryMB, &v.MaxStorageGB); {
	case errors.Is(err, sql.ErrNoRows):
		return &v, nil
	case err != nil:
		return nil, err
	}
	return &v, nil
}

// SetQuotas upserts a quotas row. Nil fields are left unchanged; a non-nil
// zero-value field clears the cap.
func (p *PGRepo) SetQuotas(ctx context.Context, userID string, qi QuotaInput, setBy string) (*QuotaView, error) {
	type col struct {
		name  string
		value *int
	}
	cols := []col{
		{"max_servers", qi.MaxServers},
		{"max_cpu_milli", qi.MaxCPUMilli},
		{"max_memory_mb", qi.MaxMemoryMB},
		{"max_storage_gb", qi.MaxStorageGB},
	}

	// Build the ON CONFLICT upsert dynamically.
	var insCols, insVals []string
	var upd []string
	var args []any
	argn := 0
	args = append(args, userID) // $1 = user_id
	argn++
	args = append(args, setBy) // $2 = updated_by
	argn++
	insCols = append(insCols, "user_id", "updated_by")
	insVals = append(insVals, "$1", "$2")

	for _, c := range cols {
		if c.value == nil {
			continue
		}
		argn++
		insCols = append(insCols, c.name)
		insVals = append(insVals, fmt.Sprintf("$%d", argn))
		args = append(args, *c.value)
		upd = append(upd, fmt.Sprintf("%s = EXCLUDED.%s", c.name, c.name))
	}

	query := fmt.Sprintf(`INSERT INTO quotas (%s) VALUES (%s)
		ON CONFLICT (user_id) DO UPDATE SET %s, updated_by = $2
		RETURNING user_id, max_servers, max_cpu_milli, max_memory_mb, max_storage_gb`,
		joinStr(insCols), joinStr(insVals), joinStr(upd))

	v := QuotaView{}
	switch err := p.db.QueryRowContext(ctx, query, args...).Scan(
		&v.UserID, &v.MaxServers, &v.MaxCPUMilli, &v.MaxMemoryMB, &v.MaxStorageGB); {
	case err != nil:
		return nil, err
	}
	return &v, nil
}

// ---- session admin ----

// ListUserSessions returns every live session for a user, newest first.
func (p *PGRepo) ListUserSessions(ctx context.Context, userID string, now time.Time) ([]SessionView, error) {
	const q = `SELECT token_hash, created_at, expires_at, revoked_at
		FROM sessions WHERE user_id = $1 AND (revoked_at IS NULL OR revoked_at > $2) AND expires_at > $2
		ORDER BY created_at DESC`
	rows, err := p.db.QueryContext(ctx, q, userID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionView
	for rows.Next() {
		var s SessionView
		if err := rows.Scan(&s.TokenHash, &s.CreatedAt, &s.ExpiresAt, &s.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeAllUserSessions marks every live session of userID revoked.
func (p *PGRepo) RevokeAllUserSessions(ctx context.Context, userID string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID)
	return err
}

// ---- account-link admin ----

// UnlinkAccount removes a single (user_id, mc_uuid) binding.
func (p *PGRepo) UnlinkAccount(ctx context.Context, userID, mcUUID string) error {
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM account_links WHERE user_id = $1 AND mc_uuid = $2`,
		userID, mcUUID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LinkAccount force-binds a UUID to a user. A UUID already linked to a different
// user → ErrConflict; same (user, uuid) pair is idempotent (ON CONFLICT DO
// NOTHING on the UNIQUE(mc_uuid) constraint, plus an idempotency check via
// EXISTS).
func (p *PGRepo) LinkAccount(ctx context.Context, userID, mcUUID, authSource string) error {
	// Check idempotency first: already linked to this user → success.
	var exists bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM account_links WHERE user_id = $1 AND mc_uuid = $2)`,
		userID, mcUUID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}

	// Try insert. The UNIQUE(mc_uuid) constraint will reject a UUID already
	// bound to a different user.
	res, err := p.db.ExecContext(ctx,
		`INSERT INTO account_links (user_id, mc_uuid, auth_source, verified_at)
		 VALUES ($1, $2, $3, now())
		 ON CONFLICT (mc_uuid) DO NOTHING`,
		userID, mcUUID, authSource)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// ---- account migration (spec §B3 inherit, scenario A) ----

// StartMigration puts a live source account into migrate mode. It supersedes any
// earlier unfinished migration for the source (so re-running /felis migrate restarts
// cleanly, invalidating a prior outstanding code) and inserts a fresh 'initiated' row,
// both under one transaction so the partial unique index never sees two live rows.
func (p *PGRepo) StartMigration(ctx context.Context, id, sourceUserID string, now time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// The source must be a live (non-deleted) account; a retired one can never
	// re-initiate a migration.
	var live bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		sourceUserID).Scan(&live); err != nil {
		return err
	}
	if !live {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_migrations WHERE source_user_id = $1 AND state <> 'redeemed'`,
		sourceUserID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_migrations (id, source_user_id, state, created_at, updated_at)
		 VALUES ($1, $2, 'initiated', $3, $3)`,
		id, sourceUserID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// MigrationForSource loads the live (non-redeemed) migration for a source, or
// ErrNotFound when none is in flight.
func (p *PGRepo) MigrationForSource(ctx context.Context, sourceUserID string) (*MigrationView, error) {
	const q = `SELECT id, source_user_id, COALESCE(target_user_id, ''), state,
		COALESCE(confirm_factor, ''), confirmed_at, code_expires_at, created_at
		FROM account_migrations
		WHERE source_user_id = $1 AND state <> 'redeemed'`
	var v MigrationView
	switch err := p.db.QueryRowContext(ctx, q, sourceUserID).Scan(
		&v.ID, &v.SourceUserID, &v.TargetUserID, &v.State,
		&v.ConfirmFactor, &v.ConfirmedAt, &v.CodeExpiresAt, &v.CreatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &v, nil
}

// ConfirmMigration advances 'initiated' → 'confirmed' for the source, stamping the
// step-up factor + time. It only advances from 'initiated' (0 rows → ErrConflict), so
// the step-up can never be replayed against a later state.
func (p *PGRepo) ConfirmMigration(ctx context.Context, sourceUserID, factor string, now time.Time) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE account_migrations
		 SET state = 'confirmed', confirm_factor = $2, confirmed_at = $3
		 WHERE source_user_id = $1 AND state = 'initiated'`,
		sourceUserID, factor, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// IssueMigrationCode advances 'confirmed' → 'code_issued', binding the target and
// storing the one-time code hash + TTL. The caller has already validated the target is
// a live account other than the source; the target FK is the backstop. Not-in-confirmed
// → ErrConflict.
func (p *PGRepo) IssueMigrationCode(ctx context.Context, sourceUserID, targetUserID, codeHash string, expiresAt time.Time) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE account_migrations
		 SET state = 'code_issued', target_user_id = $2, code_hash = $3, code_expires_at = $4
		 WHERE source_user_id = $1 AND state = 'confirmed'`,
		sourceUserID, targetUserID, codeHash, expiresAt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// RedeemMigration performs the atomic transfer + retirement in one transaction. See
// the interface doc for the full contract.
func (p *PGRepo) RedeemMigration(ctx context.Context, targetUserID, codeHash string, now time.Time) (string, []string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// Find and lock the pending migration whose named target is exactly this user. A
	// code whose target is a different user simply does not match — an intercepted
	// code is useless to a non-target. FOR UPDATE serializes concurrent redeems.
	var migID, sourceUserID string
	switch err := tx.QueryRowContext(ctx,
		`SELECT id, source_user_id FROM account_migrations
		 WHERE code_hash = $1 AND target_user_id = $2 AND state = 'code_issued'
		   AND code_expires_at > $3
		 FOR UPDATE`,
		codeHash, targetUserID, now).Scan(&migID, &sourceUserID); {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil, ErrLinkCodeInvalid
	case err != nil:
		return "", nil, err
	}

	// Re-point every server the source owns to the target, collecting the names for
	// the audit trail. Server ownership is the only thing that moves.
	rows, err := tx.QueryContext(ctx,
		`UPDATE servers SET owner_id = $2, claimed_at = now()
		 WHERE owner_id = $1 AND deleted_at IS NULL
		 RETURNING name`,
		sourceUserID, targetUserID)
	if err != nil {
		return "", nil, err
	}
	var moved []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", nil, err
		}
		moved = append(moved, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", nil, err
	}
	rows.Close()

	// Retire the source: revoke its live sessions and soft-delete it so it can neither
	// log in nor start another migration (double-spend defense). The servers just moved
	// away, so there is nothing left to release.
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		sourceUserID); err != nil {
		return "", nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET disabled = true, deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`,
		sourceUserID); err != nil {
		return "", nil, err
	}

	// Mark the migration terminal.
	if _, err := tx.ExecContext(ctx,
		`UPDATE account_migrations SET state = 'redeemed', redeemed_at = $2 WHERE id = $1`,
		migID, now); err != nil {
		return "", nil, err
	}

	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return sourceUserID, moved, nil
}

// ---- pre-session email login (spec §B) ----

// UserByEmail resolves a VERIFIED email address to its login projection, or
// ErrNotFound. Only a proven (email_verified true) address resolves, so a
// merely-asserted address never reaches a session-mintable identity. The
// account is passwordless — no password column is read.
func (p *PGRepo) UserByEmail(ctx context.Context, email string) (*StaffUser, error) {
	// lower() on both sides honors the interface's case-insensitivity contract
	// and matches the users_verified_email_unique index (lower(email)).
	const q = `SELECT id, username, COALESCE(email, ''), role::text, email_verified
		FROM users WHERE lower(email) = lower($1) AND email_verified = true`
	var u StaffUser
	switch err := p.db.QueryRowContext(ctx, q, email).Scan(
		&u.ID, &u.Username, &u.Email, &u.Role, &u.EmailVerified); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &u, nil
}

// ConsumeLoginEmailOTP redeems the live code for the PRE-SESSION email login door
// with the SAME lifecycle as VerifyEmailOTP (FOR UPDATE, expiry + attempt cap
// before the hash compare, a mismatch charges one attempt without consuming) but
// with NO identity side-effects: it neither writes users.email nor runs the
// verified-email uniqueness guard — login already resolved the userID via
// UserByEmail, which requires email_verified, so the address is settled. Errors
// are exactly ErrOTPInvalid / ErrOTPLocked (ErrEmailTaken is structurally
// impossible here).
func (p *PGRepo) ConsumeLoginEmailOTP(ctx context.Context, userID, purpose, codeHash string, now time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var (
		id         string
		storedHash string
		attempts   int
		expiresAt  time.Time
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT id, code_hash, attempts, expires_at FROM email_otps
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
		 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		userID, purpose).Scan(&id, &storedHash, &attempts, &expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		// Nothing live: never minted, already consumed, or superseded.
		return ErrOTPInvalid
	case err != nil:
		return err
	}

	if !expiresAt.After(now) {
		return ErrOTPInvalid
	}
	if attempts >= otpMaxAttempts {
		return ErrOTPLocked
	}
	if storedHash != codeHash {
		if _, err := tx.ExecContext(ctx,
			`UPDATE email_otps SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
			return fmt.Errorf("record otp attempt: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrOTPInvalid
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE email_otps SET consumed_at = $2 WHERE id = $1`, id, now); err != nil {
		return fmt.Errorf("consume otp: %w", err)
	}
	return tx.Commit()
}

// ---- op.console staff login: in-game approval state machine (spec §B op-login) ----

// CreateOpLoginRequest records a fresh pending op.console login attempt for a staff
// account. It writes the SECOND factor only — the email-OTP is minted separately
// under purpose 'op_login' — so a row here means this staff account is waiting for
// an in-game admin to vouch. email is a snapshot for the audit trail.
func (p *PGRepo) CreateOpLoginRequest(ctx context.Context, id, userID, email string, expiresAt time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO op_login_requests (id, user_id, email, expires_at) VALUES ($1, $2, $3, $4)`,
		id, userID, email, expiresAt)
	return err
}

// OpLoginRequestByID loads a request by its handle, or ErrNotFound. The status poll
// and the finish path both use it; finish additionally checks Status=='approved',
// !Consumed, and ExpiresAt>now before minting a session. Username is left empty (no
// join needed here). Status is derived from approved_at: 'approved' once set, else
// 'pending'.
func (p *PGRepo) OpLoginRequestByID(ctx context.Context, id string) (*OpLoginRequest, error) {
	const q = `SELECT id, user_id, email, expires_at, consumed_at, approved_at, approved_by
		FROM op_login_requests WHERE id = $1`
	var (
		r          OpLoginRequest
		consumedAt sql.NullTime
		approvedAt sql.NullTime
		approvedBy sql.NullString
	)
	switch err := p.db.QueryRowContext(ctx, q, id).Scan(
		&r.ID, &r.UserID, &r.Email, &r.ExpiresAt, &consumedAt, &approvedAt, &approvedBy); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	r.Consumed = consumedAt.Valid
	if approvedAt.Valid {
		r.Status = "approved"
	} else {
		r.Status = "pending"
	}
	return &r, nil
}

// ListPendingOpLogins returns the live (pending, unconsumed, unexpired at now)
// requests oldest-first, for the in-game admin's approval prompt. A resolved or
// expired request drops out of the list, so an admin only ever sees actionable
// attempts. The username is joined because the approval prompt names the staff
// account; created_at orders the list and lets the prompt show how long a request
// has been waiting.
func (p *PGRepo) ListPendingOpLogins(ctx context.Context, now time.Time) ([]OpLoginRequest, error) {
	const q = `SELECT r.id, r.user_id, u.username, r.email, r.expires_at, r.created_at
		FROM op_login_requests r JOIN users u ON u.id = r.user_id
		WHERE r.consumed_at IS NULL AND r.approved_at IS NULL AND r.expires_at > $1
		ORDER BY r.created_at`
	rows, err := p.db.QueryContext(ctx, q, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpLoginRequest
	for rows.Next() {
		var r OpLoginRequest
		if err := rows.Scan(&r.ID, &r.UserID, &r.Username, &r.Email, &r.ExpiresAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Status = "pending"
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApproveOpLogin marks a pending request approved by approverUserID (the in-game
// admin), atomically: it stamps approved_at and approved_by only WHERE the row is
// still pending, unconsumed, and unexpired at now. Zero rows affected (gone,
// already resolved, or expired) → ErrNotFound, so a double approval or an
// approval of a dead request is a no-op the caller can surface.
func (p *PGRepo) ApproveOpLogin(ctx context.Context, id, approverUserID string, now time.Time) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE op_login_requests SET approved_at = $3, approved_by = $2
		 WHERE id = $1 AND consumed_at IS NULL AND approved_at IS NULL AND expires_at > $3`,
		id, approverUserID, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ConsumeOpLoginRequest stamps consumed_at on an APPROVED, unconsumed, unexpired
// request, atomically, so it can be exchanged for a session exactly once. Zero
// rows affected (pending, already consumed, or expired) → ErrNotFound. This is the
// finish path's single-use guard; the email-OTP is consumed separately, so a lost
// race here never silently mints a second session.
func (p *PGRepo) ConsumeOpLoginRequest(ctx context.Context, id string, now time.Time) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE op_login_requests SET consumed_at = $2
		 WHERE id = $1 AND consumed_at IS NULL AND expires_at > $2`,
		id, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- setup token redemption (spec §B) ----

// ConsumeSetupToken atomically marks a one-time setup token consumed and returns
// its user_id, or ErrNotFound when the token is absent, already consumed, or
// expired. The /setup?token=... web flow redeems it for a lockdown session.
func (p *PGRepo) ConsumeSetupToken(ctx context.Context, tokenHash string, now time.Time) (string, error) {
	var userID string
	switch err := p.db.QueryRowContext(ctx,
		`UPDATE setup_tokens SET consumed_at = $2
		 WHERE token_hash = $1 AND consumed_at IS NULL AND expires_at > $2
		 RETURNING user_id`,
		tokenHash, now).Scan(&userID); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", err
	}
	return userID, nil
}

// CreateSetupToken persists a one-time first-web-login token, storing only its
// hash (the raw value rides in the /setup?token=... URL). The setup Owner-bind
// path uses CompleteOwnerSetup so identity binding, local auth, and this token
// commit atomically; this lower-level helper remains for callers that already
// established the user. The token is redeemed exactly once by ConsumeSetupToken.
func (p *PGRepo) CreateSetupToken(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO setup_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt)
	return err
}

// joinStr joins a slice of strings with ", ".
func joinStr(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	s := vals[0]
	for _, v := range vals[1:] {
		s += ", " + v
	}
	return s
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (code 23505).
func isUniqueViolation(err error) bool {
	return stringsContains(err.Error(), "duplicate key") || stringsContains(err.Error(), "23505")
}

func stringsContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
