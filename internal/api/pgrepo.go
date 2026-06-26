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
func (p *PGRepo) CreateLinkCode(ctx context.Context, code, mcUUID string, expiresAt time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO account_link_codes (code, mc_uuid, expires_at) VALUES ($1, $2, $3)`,
		code, mcUUID, expiresAt)
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
func (p *PGRepo) VerifyLinkCode(ctx context.Context, userID, code string, now time.Time) (string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var mcUUID string
	switch err := tx.QueryRowContext(ctx,
		`SELECT mc_uuid FROM account_link_codes WHERE code = $1 AND expires_at > $2`,
		code, now).Scan(&mcUUID); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrLinkCodeInvalid
	case err != nil:
		return "", err
	}

	// If this UUID is already linked, only the same user may re-verify (idempotent);
	// a different user is a conflict and must not consume the code.
	var existingUser string
	switch err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM account_links WHERE mc_uuid = $1`, mcUUID).Scan(&existingUser); {
	case errors.Is(err, sql.ErrNoRows):
		// not yet linked — fall through to insert
	case err != nil:
		return "", err
	default:
		if existingUser != userID {
			return "", ErrConflict
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_links (user_id, mc_uuid) VALUES ($1, $2)
		 ON CONFLICT (user_id, mc_uuid) DO NOTHING`, userID, mcUUID); err != nil {
		return "", fmt.Errorf("write account link: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_link_codes WHERE code = $1`, code); err != nil {
		return "", fmt.Errorf("consume link code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return mcUUID, nil
}

// QuotaAvailable treats a missing quota row or a NULL max_servers as unlimited;
// otherwise it compares the live owned-server count against the cap (spec §9.3).
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
		(s.owner_id = $1) AS owned, (s.owner_id IS NULL) AS claimable, COALESCE(s.cached_phase, '')
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

// SeedServer inserts the business rows backing a newly created server (spec
// §15): the servers row (owner_id left NULL — the server is created unowned and
// claimed later, spec §9.3) and its subdomain alias. Both inserts are
// ON CONFLICT DO NOTHING so a retried create is idempotent. The alias subdomain
// is a PRIMARY KEY, so a no-op insert means it was already bound; we then
// confirm it resolves to this server and return ErrConflict otherwise, letting
// the create handler answer 409 before it touches the CRD.
func (p *PGRepo) SeedServer(ctx context.Context, name, subdomain string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO servers (name) VALUES ($1) ON CONFLICT DO NOTHING`, name); err != nil {
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

func (p *PGRepo) Audit(ctx context.Context, e AuditEntry) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO audit_logs (actor, source, action, server_name, request_id)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''))`,
		e.Actor, e.Source, e.Action, e.ServerName, e.RequestID)
	return err
}
