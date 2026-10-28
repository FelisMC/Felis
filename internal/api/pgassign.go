package api

import (
	"context"
	"database/sql"
	"errors"
)

// quotaExcess lists each cap that one more server of size res would pass; storage is compared in MB.
func quotaExcess(c quotaCaps, count, cpuSum, memSum, storSum int64, res ResourceSpec) []QuotaExcess {
	var out []QuotaExcess
	check := func(resource string, limit sql.NullInt64, scale, after int64) {
		if limit.Valid && after > limit.Int64*scale {
			out = append(out, QuotaExcess{Resource: resource, Limit: limit.Int64 * scale, After: after})
		}
	}
	check("servers", c.MaxServers, 1, count+1)
	check("cpu_milli", c.MaxCPU, 1, cpuSum+int64(res.CPUMilli))
	check("memory_mb", c.MaxMem, 1, memSum+int64(res.MemoryMB))
	check("storage_mb", c.MaxStor, 1024, storSum+int64(res.StorageMB))
	return out
}

// assignState reads the current owner and the new owner's excess; forUpdate locks the server row.
func assignState(ctx context.Context, q rowQuerier, name, userID string, res ResourceSpec, forUpdate bool) (string, []QuotaExcess, error) {
	var live bool
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND disabled = false AND deleted_at IS NULL)`, userID).Scan(&live); err != nil {
		return "", nil, err
	}
	if !live {
		return "", nil, ErrUnknownAssignee
	}
	query := `SELECT COALESCE(owner_id, ''), retire_requested_at IS NOT NULL FROM servers WHERE name = $1 AND deleted_at IS NULL`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var owner string
	var retiring bool
	switch err := q.QueryRowContext(ctx, query, name).Scan(&owner, &retiring); {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil, ErrNotFound
	case err != nil:
		return "", nil, err
	}
	switch {
	case retiring:
		return owner, nil, ErrAssignRetiring
	case owner == userID:
		return owner, nil, ErrAlreadyOwner
	}
	caps, err := readQuota(ctx, q, userID)
	if err != nil {
		return "", nil, err
	}
	var count, cpuSum, memSum, storSum int64
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cached_cpu_milli), 0), COALESCE(SUM(cached_memory_mb), 0), COALESCE(SUM(cached_storage_mb), 0)
		 FROM servers WHERE owner_id = $1 AND deleted_at IS NULL AND name != $2`,
		userID, name).Scan(&count, &cpuSum, &memSum, &storSum); err != nil {
		return "", nil, err
	}
	return owner, quotaExcess(caps, count, cpuSum, memSum, storSum, res), nil
}

func (p *PGRepo) AssignPreview(ctx context.Context, name, userID string, res ResourceSpec) (string, []QuotaExcess, error) {
	return assignState(ctx, p.db, name, userID, res, false)
}

func (p *PGRepo) AssignServer(ctx context.Context, name, userID string, res ResourceSpec, allowOver bool) (string, []QuotaExcess, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// The same per-user lock as claims keeps the quota sums and the ownership write consistent.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, userID); err != nil {
		return "", nil, err
	}
	previous, over, err := assignState(ctx, tx, name, userID, res, true)
	if err != nil {
		return previous, nil, err
	}
	if len(over) > 0 && !allowOver {
		return previous, over, ErrQuotaExceeded
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE servers SET owner_id = $2, claimed_at = now(), last_active_at = now(), warned_3d_at = NULL, warned_1d_at = NULL, tags = '{}'
		 WHERE name = $1`, name, userID); err != nil {
		return "", nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_allowlist WHERE server_name = $1`, name); err != nil {
		return "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return previous, over, nil
}
