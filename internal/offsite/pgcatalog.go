package offsite

import (
	"context"
	"database/sql"
	"time"
)

// PGCatalog is the Catalog on the felis database.
type PGCatalog struct{ DB *sql.DB }

func (c PGCatalog) PendingWorlds(ctx context.Context) ([]WorldBackup, error) {
	return c.query(ctx, `SELECT id, server_name, backup_ref, created_at FROM world_backups
		WHERE status = 'present' AND offsite_at IS NULL ORDER BY created_at`)
}

func (c PGCatalog) PresentWorlds(ctx context.Context) ([]WorldBackup, error) {
	return c.query(ctx, `SELECT id, server_name, backup_ref, created_at FROM world_backups
		WHERE status = 'present' ORDER BY created_at`)
}

func (c PGCatalog) query(ctx context.Context, q string, args ...any) ([]WorldBackup, error) {
	rows, err := c.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorldBackup
	for rows.Next() {
		var w WorldBackup
		if err := rows.Scan(&w.ID, &w.Server, &w.Ref, &w.Created); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (c PGCatalog) MarkOffsite(ctx context.Context, id string, at time.Time) error {
	_, err := c.DB.ExecContext(ctx,
		`UPDATE world_backups SET offsite_at = $2 WHERE id = $1 AND offsite_at IS NULL`, id, at)
	return err
}

func (c PGCatalog) ExpiredRefs(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := c.DB.QueryContext(ctx, `SELECT backup_ref FROM world_backups
		WHERE status = 'deleted' AND expires_at < $1 AND offsite_at IS NOT NULL`, now)
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

// PendingCount is how many present archives wait for their copy, and since
// when the oldest has waited.
func (c PGCatalog) PendingCount(ctx context.Context) (int, time.Time, error) {
	var (
		n      int
		oldest sql.NullTime
	)
	err := c.DB.QueryRowContext(ctx, `SELECT count(*), min(created_at) FROM world_backups
		WHERE status = 'present' AND offsite_at IS NULL`).Scan(&n, &oldest)
	return n, oldest.Time, err
}
