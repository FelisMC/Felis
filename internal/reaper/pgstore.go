package reaper

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// PGStore is the production Store backed by Postgres (spec §6, §18). The SQL
// here is exercised by integration tests against a live database, not the
// hermetic reaper_test.go suite. Every statement is the narrow operation §18
// requires; there is no generic UPDATE escape hatch.
type PGStore struct {
	db *sql.DB
}

// NewPGStore wraps an existing pool (from store.PostgresDriver.DB()).
func NewPGStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

func (s *PGStore) ListActiveServers(ctx context.Context) ([]Candidate, error) {
	const q = `SELECT name, owner_id, last_active_at, warned_3d_at, warned_1d_at, retire_requested_at, retire_delete
		FROM servers WHERE deleted_at IS NULL ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var (
			c      Candidate
			owner  sql.NullString
			w3, w1 sql.NullTime
			retire sql.NullTime
		)
		if err := rows.Scan(&c.Name, &owner, &c.LastActiveAt, &w3, &w1, &retire, &c.RetireDelete); err != nil {
			return nil, err
		}
		c.OwnerID = owner.String
		if w3.Valid {
			c.Warned3dAt = w3.Time
		}
		if w1.Valid {
			c.Warned1dAt = w1.Time
		}
		if retire.Valid {
			c.RetireRequestedAt = retire.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *PGStore) FreshBackup(ctx context.Context, server string, since time.Time) (Fresh, bool, error) {
	// Only the reaper's own archives count (an idle reap's or a retirement's),
	// and only those taken since the current owner claimed the server: a manual
	// backup may predate a panel edit that did not move last_active_at, and an
	// archive from before the claim is the previous owner's world. One found corrupt is never reused.
	const q = `SELECT b.id, b.backup_ref, COALESCE(b.sha256, ''), b.offsite_at IS NOT NULL FROM world_backups b
		WHERE b.server_name = $1 AND b.status = 'present' AND b.reason IN ('inactive_15d', 'released') AND b.created_at >= $2
		  AND b.corrupt_at IS NULL
		  AND b.created_at >= COALESCE((SELECT s.claimed_at FROM servers s WHERE s.name = $1 AND s.deleted_at IS NULL), '-infinity')
		ORDER BY b.offsite_at IS NOT NULL DESC, b.created_at DESC LIMIT 1`
	var f Fresh
	switch err := s.db.QueryRowContext(ctx, q, server, since).Scan(&f.ID, &f.Ref, &f.SHA256, &f.Offsite); {
	case err == sql.ErrNoRows:
		return Fresh{}, false, nil
	case err != nil:
		return Fresh{}, false, err
	}
	return f, true, nil
}

func (s *PGStore) InsertBackup(ctx context.Context, rec BackupRecord) error {
	const q = `INSERT INTO world_backups
		(id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at, sha256, skipped_entries)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, 'present', now(), $7, NULLIF($8, ''), $9)`
	_, err := s.db.ExecContext(ctx, q,
		rec.ID, rec.ServerName, rec.FormerOwner, rec.BackupRef, rec.SizeBytes, rec.Reason, rec.ExpiresAt,
		rec.SHA256, rec.SkippedEntries)
	return err
}

// ReleaseWorld releases ownership and resets the activity clock and warnings —
// without deleting the row (red line ②). The resource cache stays: the server
// keeps its spec, an ownerless row is in nobody's quota sum, and the next claim is
// gated on that size and counts it. The wake allowlist is emptied in the same
// statement: its players were vouched for by the owner being released, and an
// ownerless server set to autostartPolicy=allowlist would otherwise stay
// wakeable by them. A pending release is done with once the world is gone;
// a pending deletion stays, so an idle reap that lands on a server an admin is
// deleting leaves the deletion for the next run.
func (s *PGStore) ReleaseWorld(ctx context.Context, name string, at time.Time) error {
	const q = `WITH released AS (
		UPDATE servers
		SET owner_id = NULL, last_active_at = $2, warned_3d_at = NULL, warned_1d_at = NULL,
		    retire_requested_at = CASE WHEN retire_delete THEN retire_requested_at END
		WHERE name = $1 AND deleted_at IS NULL RETURNING name)
		DELETE FROM server_allowlist WHERE server_name IN (SELECT name FROM released)`
	_, err := s.db.ExecContext(ctx, q, name, at)
	return err
}

// DeleteServerRow marks a server deleted once its MinecraftServer is gone. The
// row stays (red line ②) and so do its backups (red line ③), recorded against
// the name; the aliases go, which frees the subdomain, and the allowlist with
// them. A create under the name later starts the row over (api.SeedServer).
func (s *PGStore) DeleteServerRow(ctx context.Context, name string, at time.Time) error {
	const q = `WITH gone AS (
		UPDATE servers
		SET deleted_at = $2, owner_id = NULL, retire_requested_at = NULL, retire_delete = false,
		    warned_3d_at = NULL, warned_1d_at = NULL
		WHERE name = $1 AND deleted_at IS NULL AND retire_delete RETURNING name),
	aliases AS (DELETE FROM server_aliases WHERE server_name IN (SELECT name FROM gone))
	DELETE FROM server_allowlist WHERE server_name IN (SELECT name FROM gone)`
	_, err := s.db.ExecContext(ctx, q, name, at)
	return err
}

func (s *PGStore) RestartClock(ctx context.Context, name string, at time.Time) error {
	const q = `UPDATE servers SET last_active_at = $2, warned_3d_at = NULL, warned_1d_at = NULL
		WHERE name = $1 AND deleted_at IS NULL`
	_, err := s.db.ExecContext(ctx, q, name, at)
	return err
}

func (s *PGStore) MarkWarned(ctx context.Context, name string, tier Tier, at time.Time) error {
	// The column is one of two fixed identifiers, never user input.
	col := "warned_3d_at"
	if tier == Tier1d {
		col = "warned_1d_at"
	}
	q := fmt.Sprintf(`UPDATE servers SET %s = $2 WHERE name = $1 AND deleted_at IS NULL`, col)
	_, err := s.db.ExecContext(ctx, q, name, at)
	return err
}

func (s *PGStore) PresentBackupBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0) FROM world_backups WHERE status = 'present'`).Scan(&n)
	return n, err
}

func (s *PGStore) EvictableBackups(ctx context.Context) ([]StoredBackup, error) {
	const q = `SELECT id, server_name, backup_ref, size_bytes, reason, COALESCE(sha256, '') FROM world_backups
		WHERE status = 'present' AND (reason NOT IN ('inactive_15d', 'released') OR offsite_at IS NOT NULL)
		ORDER BY reason IN ('inactive_15d', 'released'), created_at ASC`
	return s.queryBackups(ctx, q)
}

// ExcessBackups lists the present backups of one reason that owner holds of
// server beyond the newest keep, oldest first: what the backup Job removes after
// adding one. Another owner's backups of the same server (one it held before the
// world was reaped and claimed again) are theirs to restore and never counted.
// protect, when set, is a backup id left out of the list whatever its age (the
// one a chained restore is about to extract).
func (s *PGStore) ExcessBackups(ctx context.Context, server, owner, reason string, keep int, protect string) ([]StoredBackup, error) {
	const q = `SELECT id, server_name, backup_ref, size_bytes, reason, COALESCE(sha256, '') FROM world_backups
		WHERE server_name = $1 AND COALESCE(former_owner, '') = $5 AND status = 'present' AND reason = $2 AND id <> $4
		ORDER BY corrupt_at IS NULL DESC, created_at DESC OFFSET $3`
	out, err := s.queryBackups(ctx, q, server, reason, keep, protect, owner)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}

func (s *PGStore) ListExpiredBackups(ctx context.Context, now time.Time) ([]StoredBackup, error) {
	const q = `SELECT id, server_name, backup_ref, size_bytes, reason, COALESCE(sha256, '') FROM world_backups
		WHERE (status = 'present' AND expires_at < $1) OR status = 'expired' ORDER BY expires_at ASC`
	return s.queryBackups(ctx, q, now)
}

func (s *PGStore) queryBackups(ctx context.Context, q string, args ...any) ([]StoredBackup, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredBackup
	for rows.Next() {
		var b StoredBackup
		if err := rows.Scan(&b.ID, &b.ServerName, &b.BackupRef, &b.SizeBytes, &b.Reason, &b.SHA256); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *PGStore) BackupsToVerify(ctx context.Context, checkedBefore time.Time, limit int) ([]StoredBackup, error) {
	const q = `SELECT id, server_name, backup_ref, size_bytes, reason, COALESCE(sha256, '') FROM world_backups
		WHERE status = 'present' AND corrupt_at IS NULL AND (verified_at IS NULL OR verified_at < $1)
		ORDER BY verified_at NULLS FIRST, created_at LIMIT $2`
	return s.queryBackups(ctx, q, checkedBefore, limit)
}

func (s *PGStore) MarkBackupVerified(ctx context.Context, id, sha256 string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE world_backups SET verified_at = $3, sha256 = COALESCE(sha256, NULLIF($2, '')) WHERE id = $1`,
		id, sha256, at)
	return err
}

func (s *PGStore) MarkBackupCorrupt(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE world_backups SET corrupt_at = $2 WHERE id = $1 AND corrupt_at IS NULL`, id, at)
	return err
}

func (s *PGStore) LiveBackupRefs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT backup_ref FROM world_backups WHERE status <> 'deleted'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func (s *PGStore) MarkBackupDeleted(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE world_backups SET status = 'deleted', deleted_at = $2 WHERE id = $1`, id, at)
	return err
}

// Audit writes a reaper-sourced row. The actor/source are the system identity
// "reaper" (no human Access email applies, spec §14); former_owner has no
// dedicated column so it goes into the payload jsonb.
func (s *PGStore) Audit(ctx context.Context, rec AuditRecord) error {
	payload := []byte("{}")
	if rec.FormerOwner != "" {
		p, err := json.Marshal(map[string]string{"former_owner": rec.FormerOwner})
		if err != nil {
			return err
		}
		payload = p
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_logs (actor, source, action, server_name, payload)
		 VALUES ('reaper', 'reaper', $1, NULLIF($2, ''), $3)`,
		rec.Action, rec.ServerName, string(payload))
	return err
}
