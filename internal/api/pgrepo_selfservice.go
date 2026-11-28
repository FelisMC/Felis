package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrSelfServiceDisabled = errors.New("self-service server creation is not enabled for this user")

// quotaCaps with no source permits unlimited claims but disables self-service creation.
type quotaCaps struct {
	MaxServers, MaxCPU, MaxMem, MaxStor sql.NullInt64
	Source                              string
}

// readQuota applies the enabled default to claims and resizes as well as creation when the user has no quota row.
func readQuota(ctx context.Context, q rowQuerier, userID string) (quotaCaps, error) {
	var c quotaCaps
	switch err := q.QueryRowContext(ctx,
		`SELECT max_servers, max_cpu_milli, max_memory_mb, max_storage_gb
		 FROM quotas WHERE user_id = $1`, userID).Scan(&c.MaxServers, &c.MaxCPU, &c.MaxMem, &c.MaxStor); {
	case err == nil:
		c.Source = "user"
		return c, nil
	case !errors.Is(err, sql.ErrNoRows):
		return c, err
	}
	var raw []byte
	switch err := q.QueryRowContext(ctx,
		`SELECT value FROM platform_settings WHERE key = $1`, selfServiceQuotaKey).Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return c, nil
	case err != nil:
		return c, err
	}
	var d selfServiceQuota
	if err := json.Unmarshal(raw, &d); err != nil {
		return c, fmt.Errorf("default quota setting: %w", err)
	}
	if !d.Enabled {
		return c, nil
	}
	set := func(v *int) sql.NullInt64 {
		if v == nil {
			return sql.NullInt64{}
		}
		return sql.NullInt64{Int64: int64(*v), Valid: true}
	}
	c.MaxServers, c.MaxCPU, c.MaxMem, c.MaxStor = set(d.MaxServers), set(d.MaxCPUMilli), set(d.MaxMemoryMB), set(d.MaxStorageGB)
	c.Source = "default"
	return c, nil
}

func (p *PGRepo) SelfServiceState(ctx context.Context, userID string) (QuotaState, error) {
	caps, err := readQuota(ctx, p.db, userID)
	if err != nil {
		return QuotaState{}, err
	}
	st := QuotaState{Source: caps.Source}
	if caps.Source != "" {
		st.Limits = &QuotaInput{
			MaxServers: nullToPtr(caps.MaxServers), MaxCPUMilli: nullToPtr(caps.MaxCPU),
			MaxMemoryMB: nullToPtr(caps.MaxMem), MaxStorageGB: nullToPtr(caps.MaxStor),
		}
	}
	err = p.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cached_cpu_milli), 0), COALESCE(SUM(cached_memory_mb), 0), COALESCE(SUM(cached_storage_mb), 0)
		 FROM servers WHERE owner_id = $1 AND deleted_at IS NULL`, userID).Scan(
		&st.Usage.Servers, &st.Usage.CPUMilli, &st.Usage.MemoryMB, &st.Usage.StorageMB)
	return st, err
}

func nullToPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func (p *PGRepo) SeedOwnedServer(ctx context.Context, name, subdomain, userID string, res ResourceSpec) (time.Time, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, userID); err != nil {
		return time.Time{}, err
	}
	caps, err := readQuota(ctx, tx, userID)
	if err != nil {
		return time.Time{}, err
	}
	if caps.Source == "" {
		return time.Time{}, ErrSelfServiceDisabled
	}
	var count, cpuSum, memSum, storSum int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cached_cpu_milli), 0), COALESCE(SUM(cached_memory_mb), 0), COALESCE(SUM(cached_storage_mb), 0)
		 FROM servers WHERE owner_id = $1 AND deleted_at IS NULL AND name != $2`,
		userID, name).Scan(&count, &cpuSum, &memSum, &storSum); err != nil {
		return time.Time{}, err
	}
	if !quotaAllows(caps.MaxServers, caps.MaxCPU, caps.MaxMem, caps.MaxStor, count, cpuSum, memSum, storSum, 1, res) {
		return time.Time{}, ErrQuotaExceeded
	}
	var claimedAt time.Time
	err = tx.QueryRowContext(ctx,
		`INSERT INTO servers (name, owner_id, claimed_at, cached_cpu_milli, cached_memory_mb, cached_storage_mb)
		 VALUES ($1, $2, clock_timestamp(), $3, $4, $5)
		 ON CONFLICT (name) DO UPDATE SET owner_id = EXCLUDED.owner_id, claimed_at = EXCLUDED.claimed_at, last_active_at = now(),
		   warned_3d_at = NULL, warned_1d_at = NULL, cached_phase = NULL, created_at = now(), deleted_at = NULL,
		   retire_requested_at = NULL, retire_delete = false, protected = false, tags = '{}',
		   cached_cpu_milli = EXCLUDED.cached_cpu_milli, cached_memory_mb = EXCLUDED.cached_memory_mb,
		   cached_storage_mb = EXCLUDED.cached_storage_mb
		 WHERE servers.deleted_at IS NOT NULL
		 RETURNING claimed_at`,
		name, userID, res.CPUMilli, res.MemoryMB, res.StorageMB).Scan(&claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrServerExists
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("seed server row: %w", err)
	}
	for _, q := range []string{
		`DELETE FROM server_allowlist WHERE server_name = $1`,
		`DELETE FROM server_collaborators WHERE server_name = $1`,
		`DELETE FROM server_aliases WHERE server_name = $1`,
		`DELETE FROM server_schedules WHERE server_name = $1`,
	} {
		if _, err := tx.ExecContext(ctx, q, name); err != nil {
			return time.Time{}, fmt.Errorf("clear an earlier server's rows: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_aliases (subdomain, server_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		subdomain, name); err != nil {
		return time.Time{}, fmt.Errorf("seed subdomain alias: %w", err)
	}
	var boundTo string
	if err := tx.QueryRowContext(ctx,
		`SELECT server_name FROM server_aliases WHERE subdomain = $1`, subdomain).Scan(&boundTo); err != nil {
		return time.Time{}, fmt.Errorf("confirm subdomain alias: %w", err)
	}
	if boundTo != name {
		return time.Time{}, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return claimedAt, nil
}

func (p *PGRepo) DiscardSeededServer(ctx context.Context, name, ownerID string, claimedAt time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	var res sql.Result
	if ownerID != "" {
		res, err = tx.ExecContext(ctx,
			`UPDATE servers SET deleted_at = now() WHERE name = $1 AND deleted_at IS NULL AND owner_id = $2 AND claimed_at = $3`,
			name, ownerID, claimedAt)
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE servers SET deleted_at = now() WHERE name = $1 AND deleted_at IS NULL`, name)
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM server_aliases WHERE server_name = $1`, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}
