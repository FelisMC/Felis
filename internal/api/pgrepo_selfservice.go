package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrSelfServiceDisabled means a user asked to create a server with neither a
// quota of their own nor a default quota the Owner has turned on.
var ErrSelfServiceDisabled = errors.New("self-service server creation is not enabled for this user")

// quotaCaps is the quota in force for one user, and where it comes from: "user"
// for their own quotas row, "default" for the Owner's default quota, "" for
// none (unlimited for a claim; no self-service creation).
type quotaCaps struct {
	MaxServers, MaxCPU, MaxMem, MaxStor sql.NullInt64
	Source                              string
}

// readQuota reads the quota in force for userID: their row in quotas, else the
// default quota when the Owner has turned it on. The default covers every user
// without a row of their own, for claims and resizes as much as for creation,
// so a user who created a server under it cannot grow past it afterwards.
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

// SelfServiceState reads the quota in force for userID and what their servers
// use of it.
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

// SeedOwnedServer is SeedServer for a server a user creates for themselves: in
// the same transaction it takes the user's claim lane (the advisory lock
// ClaimServer and ResizeServer take), checks the new server against the quota in
// force, and writes the row already owned by them. Two creates by one user
// therefore serialize, and both cannot pass a cap only one of them fits under.
// ErrSelfServiceDisabled when the user has no quota at all, ErrQuotaExceeded when
// the server does not fit, ErrConflict when the subdomain is bound elsewhere.
func (p *PGRepo) SeedOwnedServer(ctx context.Context, name, subdomain, userID string, res ResourceSpec) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, userID); err != nil {
		return err
	}
	caps, err := readQuota(ctx, tx, userID)
	if err != nil {
		return err
	}
	if caps.Source == "" {
		return ErrSelfServiceDisabled
	}
	var count, cpuSum, memSum, storSum int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cached_cpu_milli), 0), COALESCE(SUM(cached_memory_mb), 0), COALESCE(SUM(cached_storage_mb), 0)
		 FROM servers WHERE owner_id = $1 AND deleted_at IS NULL AND name != $2`,
		userID, name).Scan(&count, &cpuSum, &memSum, &storSum); err != nil {
		return err
	}
	if !quotaAllows(caps.MaxServers, caps.MaxCPU, caps.MaxMem, caps.MaxStor, count, cpuSum, memSum, storSum, 1, res) {
		return ErrQuotaExceeded
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO servers (name, owner_id, claimed_at, cached_cpu_milli, cached_memory_mb, cached_storage_mb)
		 VALUES ($1, $2, now(), $3, $4, $5)
		 ON CONFLICT (name) DO UPDATE SET owner_id = EXCLUDED.owner_id, claimed_at = now(), last_active_at = now(),
		   warned_3d_at = NULL, warned_1d_at = NULL, cached_phase = NULL, created_at = now(), deleted_at = NULL,
		   retire_requested_at = NULL, retire_delete = false,
		   cached_cpu_milli = EXCLUDED.cached_cpu_milli, cached_memory_mb = EXCLUDED.cached_memory_mb,
		   cached_storage_mb = EXCLUDED.cached_storage_mb`,
		name, userID, res.CPUMilli, res.MemoryMB, res.StorageMB); err != nil {
		return fmt.Errorf("seed server row: %w", err)
	}
	for _, q := range []string{
		`DELETE FROM server_allowlist WHERE server_name = $1`,
		`DELETE FROM server_aliases WHERE server_name = $1`,
		`DELETE FROM server_schedules WHERE server_name = $1`,
	} {
		if _, err := tx.ExecContext(ctx, q, name); err != nil {
			return fmt.Errorf("clear an earlier server's rows: %w", err)
		}
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

// DiscardSeededServer undoes SeedOwnedServer when the cluster then refused the
// server: the row is marked deleted and its alias released, so the failed create
// neither counts against the user's quota nor holds the subdomain.
func (p *PGRepo) DiscardSeededServer(ctx context.Context, name string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.ExecContext(ctx, `UPDATE servers SET deleted_at = now() WHERE name = $1 AND deleted_at IS NULL`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_aliases WHERE server_name = $1`, name); err != nil {
		return err
	}
	return tx.Commit()
}
