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
	const q = `SELECT name, owner_id, last_active_at, warned_3d_at, warned_1d_at
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
		)
		if err := rows.Scan(&c.Name, &owner, &c.LastActiveAt, &w3, &w1); err != nil {
			return nil, err
		}
		c.OwnerID = owner.String
		if w3.Valid {
			c.Warned3dAt = w3.Time
		}
		if w1.Valid {
			c.Warned1dAt = w1.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *PGStore) FreshBackup(ctx context.Context, server string, since time.Time) (string, bool, error) {
	const q = `SELECT backup_ref FROM world_backups
		WHERE server_name = $1 AND status = 'present' AND created_at >= $2
		ORDER BY created_at DESC LIMIT 1`
	var ref string
	switch err := s.db.QueryRowContext(ctx, q, server, since).Scan(&ref); {
	case err == sql.ErrNoRows:
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return ref, true, nil
}

func (s *PGStore) InsertBackup(ctx context.Context, rec BackupRecord) error {
	const q = `INSERT INTO world_backups
		(id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, 'present', now(), $7)`
	_, err := s.db.ExecContext(ctx, q,
		rec.ID, rec.ServerName, rec.FormerOwner, rec.BackupRef, rec.SizeBytes, rec.Reason, rec.ExpiresAt)
	return err
}

// ReleaseWorld releases ownership and resets the activity clock and warnings —
// without deleting the row (red line ②).
func (s *PGStore) ReleaseWorld(ctx context.Context, name string, at time.Time) error {
	const q = `UPDATE servers
		SET owner_id = NULL, last_active_at = $2, warned_3d_at = NULL, warned_1d_at = NULL
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

func (s *PGStore) OldestPresentBackups(ctx context.Context) ([]StoredBackup, error) {
	const q = `SELECT id, server_name, backup_ref, size_bytes FROM world_backups
		WHERE status = 'present' ORDER BY created_at ASC`
	return s.queryBackups(ctx, q)
}

func (s *PGStore) ListExpiredBackups(ctx context.Context, now time.Time) ([]StoredBackup, error) {
	const q = `SELECT id, server_name, backup_ref, size_bytes FROM world_backups
		WHERE status = 'present' AND expires_at < $1 ORDER BY expires_at ASC`
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
		if err := rows.Scan(&b.ID, &b.ServerName, &b.BackupRef, &b.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, b)
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
