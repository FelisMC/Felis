package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PGRepo uses supplied API timestamps for writes; only updated_at triggers use the database clock.
type PGRepo struct {
	db *sql.DB
}

func NewPGRepo(db *sql.DB) *PGRepo { return &PGRepo{db: db} }

func (p *PGRepo) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }

func (p *PGRepo) ServerBySubdomain(ctx context.Context, subdomain string) (*ServerRecord, error) {
	const q = `SELECT s.name, sa.subdomain, COALESCE(s.owner_id, ''), COALESCE(s.cached_phase, ''),
		s.retire_requested_at, s.retire_delete
		FROM server_aliases sa JOIN servers s ON s.name = sa.server_name
		WHERE sa.subdomain = $1 AND s.deleted_at IS NULL`
	var r ServerRecord
	var retireAt sql.NullTime
	var retireDelete bool
	switch err := p.db.QueryRowContext(ctx, q, subdomain).Scan(&r.Name, &r.Subdomain, &r.OwnerID, &r.CachedPhase, &retireAt, &retireDelete); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	r.Retire = retireState(retireAt, retireDelete)
	return &r, nil
}

func (p *PGRepo) ServerByName(ctx context.Context, name string) (*ServerRecord, error) {
	const q = `SELECT s.name, COALESCE(sa.subdomain, ''), COALESCE(s.owner_id, ''), COALESCE(s.cached_phase, ''),
		s.retire_requested_at, s.retire_delete
		FROM servers s LEFT JOIN server_aliases sa ON sa.server_name = s.name
		WHERE s.name = $1 AND s.deleted_at IS NULL`
	var r ServerRecord
	var retireAt sql.NullTime
	var retireDelete bool
	switch err := p.db.QueryRowContext(ctx, q, name).Scan(&r.Name, &r.Subdomain, &r.OwnerID, &r.CachedPhase, &retireAt, &retireDelete); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	r.Retire = retireState(retireAt, retireDelete)
	return &r, nil
}

func retireState(at sql.NullTime, deleteServer bool) *RetireState {
	if !at.Valid {
		return nil
	}
	return &RetireState{RequestedAt: at.Time, Delete: deleteServer}
}

func (p *PGRepo) IsLinked(ctx context.Context, userID string) (bool, error) {
	var ok bool
	err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM account_links WHERE user_id = $1)`, userID).Scan(&ok)
	return ok, err
}

func (p *PGRepo) CreateLinkCode(ctx context.Context, code, mcUUID, authSource string, expiresAt time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO account_link_codes (code, mc_uuid, auth_source, expires_at) VALUES ($1, $2, $3, $4)`,
		code, mcUUID, authSource, expiresAt)
	return err
}

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

	// A link owned by a soft-deleted account is taken over; a disabled account's link stays a conflict
	// so a lockout cannot be bypassed.
	var existingUser string
	switch err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM account_links WHERE mc_uuid = $1`, mcUUID).Scan(&existingUser); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", "", err
	default:
		if existingUser != userID {
			var linkedDeleted bool
			if err := tx.QueryRowContext(ctx,
				`SELECT deleted_at IS NOT NULL FROM users WHERE id = $1`,
				existingUser).Scan(&linkedDeleted); err != nil {
				return "", "", err
			}
			if !linkedDeleted {
				return "", "", ErrConflict
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE account_links SET user_id = $1, auth_source = $2, verified_at = $4
				  WHERE mc_uuid = $3`,
				userID, authSource, mcUUID, now); err != nil {
				return "", "", fmt.Errorf("take over retired link: %w", err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_links (user_id, mc_uuid, auth_source, verified_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (user_id, mc_uuid) DO UPDATE SET auth_source = EXCLUDED.auth_source`,
		userID, mcUUID, authSource, now); err != nil {
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

func (p *PGRepo) RedeemPlayerBindCode(ctx context.Context, newUserID, code string, now time.Time) (string, string, string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var mcUUID, authSource string
	switch err := tx.QueryRowContext(ctx,
		`SELECT mc_uuid, auth_source FROM account_link_codes WHERE code = $1 AND expires_at > $2
		 FOR UPDATE`,
		code, now).Scan(&mcUUID, &authSource); {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", "", ErrLinkCodeInvalid
	case err != nil:
		return "", "", "", err
	}
	userID := newUserID
	var existingRole string
	var disabled, deleted bool
	switch err := tx.QueryRowContext(ctx,
		`SELECT u.id, u.role::text, u.disabled, u.deleted_at IS NOT NULL
		   FROM account_links al JOIN users u ON u.id = al.user_id WHERE al.mc_uuid = $1`,
		mcUUID).Scan(&userID, &existingRole, &disabled, &deleted); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, username, role, created_at) VALUES ($1, $2, 'user', $3)
			 ON CONFLICT (username) DO NOTHING`,
			newUserID, mcUUID, now); err != nil {
			return "", "", "", fmt.Errorf("create player: %w", err)
		}
		// Re-read by username so a concurrent winner's row is used and a deleted row squatting it is refused.
		var role string
		var dis, del bool
		if err := tx.QueryRowContext(ctx,
			`SELECT id, role::text, disabled, deleted_at IS NOT NULL FROM users WHERE username = $1`,
			mcUUID).Scan(&userID, &role, &dis, &del); err != nil {
			return "", "", "", fmt.Errorf("create player: %w", err)
		}
		if role != "user" {
			return "", "", "", ErrPlayerBindForbidden
		}
		if dis || del {
			return "", "", "", ErrPlayerAccountRetired
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO account_links (user_id, mc_uuid, auth_source, verified_at) VALUES ($1, $2, $3, $4)
			 ON CONFLICT (mc_uuid) DO NOTHING`,
			userID, mcUUID, authSource, now); err != nil {
			return "", "", "", fmt.Errorf("write account link: %w", err)
		}
	case err != nil:
		return "", "", "", err
	default:
		if existingRole != "user" {
			return "", "", "", ErrPlayerBindForbidden
		}
		if disabled || deleted {
			return "", "", "", ErrPlayerAccountRetired
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

// CompleteOwnerSetup creates the first owner and setup token; an owner with an existing login factor causes ErrConflict.
func (p *PGRepo) CompleteOwnerSetup(ctx context.Context, newUserID string, now time.Time,
	tokenHash string, tokenExpiresAt time.Time) (string, string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// Serializes first-run creation and link renewal even when no user row exists to lock.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('felis.owner_setup'))`); err != nil {
		return "", "", err
	}
	userID, username := newUserID, "owner"
	var onboarded bool
	err = tx.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.email_verified OR EXISTS (
		   SELECT 1 FROM webauthn_credentials c WHERE c.user_id = u.id)
		 FROM users u WHERE u.role IN ('owner', 'admin') AND u.deleted_at IS NULL
		 ORDER BY (u.role = 'owner') DESC, u.created_at, u.id LIMIT 1 FOR UPDATE`,
	).Scan(&userID, &username, &onboarded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (id, username, role, created_at) VALUES ($1, $2, 'owner', $3)`,
			newUserID, username, now); err != nil {
			return "", "", fmt.Errorf("create owner: %w", err)
		}
	case err != nil:
		return "", "", err
	case onboarded:
		return userID, username, ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO platform_settings (key, value, updated_at) VALUES ($1, $2::jsonb, $3)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		LocalAuthEnabledKey, "true", now); err != nil {
		return "", "", fmt.Errorf("enable local auth: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM setup_tokens WHERE user_id = $1`, userID); err != nil {
		return "", "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO setup_tokens (token_hash, user_id, expires_at, created_at) VALUES ($1, $2, $3, $4)`,
		tokenHash, userID, tokenExpiresAt, now); err != nil {
		return "", "", fmt.Errorf("mint setup token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return userID, username, nil
}

// QuotaCheck is advisory; ClaimServer and ResizeServer repeat the check under the user's lock.
func (p *PGRepo) QuotaCheck(ctx context.Context, userID string, excludeName string, incoming ResourceSpec) (bool, error) {
	return quotaFits(ctx, p.db, userID, excludeName, incoming)
}

func quotaFits(ctx context.Context, q rowQuerier, userID, excludeName string, incoming ResourceSpec) (bool, error) {
	caps, err := readQuota(ctx, q, userID)
	if err != nil {
		return false, err
	}
	if caps.Source == "" {
		return true, nil
	}
	maxServers, maxCPU, maxMem, maxStor := caps.MaxServers, caps.MaxCPU, caps.MaxMem, caps.MaxStor

	var count, cpuSum, memSum, storSum int64
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cached_cpu_milli), 0), COALESCE(SUM(cached_memory_mb), 0), COALESCE(SUM(cached_storage_mb), 0)
		 FROM servers WHERE owner_id = $1 AND deleted_at IS NULL AND name != $2`,
		userID, excludeName).Scan(&count, &cpuSum, &memSum, &storSum); err != nil {
		return false, err
	}

	return quotaAllows(maxServers, maxCPU, maxMem, maxStor, count, cpuSum, memSum, storSum, 1, incoming), nil
}

func (p *PGRepo) UpdateServerResources(ctx context.Context, name string, cpuMilli, memoryMB, storageMB int) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE servers SET cached_cpu_milli = $2, cached_memory_mb = $3, cached_storage_mb = $4 WHERE name = $1 AND deleted_at IS NULL`,
		name, cpuMilli, memoryMB, storageMB)
	return err
}

// ResizeServer locks the owner's lane before the server row and retries if ownership changes.
func (p *PGRepo) ResizeServer(ctx context.Context, name string, cpuMilli, memoryMB int) (ResourceSpec, error) {
	for attempt := 1; ; attempt++ {
		prev, moved, err := p.resizeServer(ctx, name, cpuMilli, memoryMB)
		if !moved {
			return prev, err
		}
		if attempt == 3 {
			return ResourceSpec{}, fmt.Errorf("resize %s: its owner changed three times while it waited", name)
		}
	}
}

// resizeServer reports moved when ownership changed without writing anything.
func (p *PGRepo) resizeServer(ctx context.Context, name string, cpuMilli, memoryMB int) (prev ResourceSpec, moved bool, err error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return ResourceSpec{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var owner sql.NullString
	switch err := tx.QueryRowContext(ctx,
		`SELECT owner_id FROM servers WHERE name = $1 AND deleted_at IS NULL`, name).Scan(&owner); {
	case errors.Is(err, sql.ErrNoRows):
		return ResourceSpec{}, false, nil
	case err != nil:
		return ResourceSpec{}, false, err
	}
	if owner.Valid {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, owner.String); err != nil {
			return ResourceSpec{}, false, err
		}
	}
	var locked sql.NullString
	switch err := tx.QueryRowContext(ctx,
		`SELECT owner_id, cached_cpu_milli, cached_memory_mb, cached_storage_mb
		 FROM servers WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`,
		name).Scan(&locked, &prev.CPUMilli, &prev.MemoryMB, &prev.StorageMB); {
	case errors.Is(err, sql.ErrNoRows):
		return ResourceSpec{}, false, nil
	case err != nil:
		return ResourceSpec{}, false, err
	}
	if locked != owner {
		return ResourceSpec{}, true, nil
	}

	if cpuMilli > prev.CPUMilli || memoryMB > prev.MemoryMB {
		ok, err := quotaFits(ctx, tx, owner.String, name,
			ResourceSpec{CPUMilli: cpuMilli, MemoryMB: memoryMB, StorageMB: prev.StorageMB})
		if err != nil {
			return ResourceSpec{}, false, err
		}
		if !ok {
			return ResourceSpec{}, false, ErrQuotaExceeded
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE servers SET cached_cpu_milli = $2, cached_memory_mb = $3 WHERE name = $1`,
		name, cpuMilli, memoryMB); err != nil {
		return ResourceSpec{}, false, err
	}
	return prev, false, tx.Commit()
}

func (p *PGRepo) ClaimServer(ctx context.Context, name, userID string) (bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// Serializes this user's claims so the quota aggregate and the ownership write see one state.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, userID); err != nil {
		return false, err
	}

	var owned sql.NullString
	var retiring bool
	var cpu, mem, stor int
	switch err := tx.QueryRowContext(ctx,
		`SELECT owner_id, retire_requested_at IS NOT NULL, cached_cpu_milli, cached_memory_mb, cached_storage_mb
		 FROM servers WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`,
		name).Scan(&owned, &retiring, &cpu, &mem, &stor); {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrNotFound
	case err != nil:
		return false, err
	}
	if owned.Valid {
		return false, nil
	}
	if retiring {
		return false, nil
	}

	caps, err := readQuota(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	maxServers, maxCPU, maxMem, maxStor := caps.MaxServers, caps.MaxCPU, caps.MaxMem, caps.MaxStor
	var count, cpuSum, memSum, storSum int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cached_cpu_milli), 0), COALESCE(SUM(cached_memory_mb), 0), COALESCE(SUM(cached_storage_mb), 0)
		 FROM servers WHERE owner_id = $1 AND deleted_at IS NULL AND name != $2`,
		userID, name).Scan(&count, &cpuSum, &memSum, &storSum); err != nil {
		return false, err
	}
	if !quotaAllows(maxServers, maxCPU, maxMem, maxStor, count, cpuSum, memSum, storSum,
		1, ResourceSpec{CPUMilli: cpu, MemoryMB: mem, StorageMB: stor}) {
		return false, ErrQuotaExceeded
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE servers SET owner_id = $2, claimed_at = now(), last_active_at = now(), warned_3d_at = NULL, warned_1d_at = NULL
		 WHERE name = $1 AND owner_id IS NULL AND deleted_at IS NULL AND retire_requested_at IS NULL`,
		name, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_allowlist WHERE server_name = $1`, name); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// quotaAllows treats invalid caps as unlimited; storage caps use GB.
func quotaAllows(maxServers, maxCPU, maxMem, maxStor sql.NullInt64,
	count, cpuSum, memSum, storSum, incomingCount int64, incoming ResourceSpec) bool {
	if maxServers.Valid && count+incomingCount > maxServers.Int64 {
		return false
	}
	if maxCPU.Valid && cpuSum+int64(incoming.CPUMilli) > maxCPU.Int64 {
		return false
	}
	if maxMem.Valid && memSum+int64(incoming.MemoryMB) > maxMem.Int64 {
		return false
	}
	if maxStor.Valid && storSum+int64(incoming.StorageMB) > maxStor.Int64*1024 {
		return false
	}
	return true
}

func (p *PGRepo) UserInAllowlist(ctx context.Context, name, userID string) (bool, error) {
	const q = `SELECT EXISTS(
		SELECT 1 FROM server_allowlist sa JOIN account_links al ON al.mc_uuid = sa.mc_uuid
		WHERE sa.server_name = $1 AND al.user_id = $2 AND sa.revoked_at IS NULL)`
	var ok bool
	err := p.db.QueryRowContext(ctx, q, name, userID).Scan(&ok)
	return ok, err
}

func (p *PGRepo) UUIDInAllowlist(ctx context.Context, name, mcUUID string) (bool, error) {
	const q = `SELECT EXISTS(
		SELECT 1 FROM server_allowlist WHERE server_name = $1 AND mc_uuid = $2 AND revoked_at IS NULL)`
	var ok bool
	err := p.db.QueryRowContext(ctx, q, name, mcUUID).Scan(&ok)
	return ok, err
}

func (p *PGRepo) ServerAllowlist(ctx context.Context, name string) ([]AllowlistEntry, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT sa.mc_uuid::text, COALESCE(u.username, ''), sa.added_at, sa.revoked_at IS NULL
		   FROM server_allowlist sa
		   LEFT JOIN account_links al ON al.mc_uuid = sa.mc_uuid
		   LEFT JOIN users u ON u.id = al.user_id AND u.deleted_at IS NULL
		  WHERE sa.server_name = $1
		  ORDER BY sa.added_at DESC, sa.mc_uuid`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AllowlistEntry{}
	for rows.Next() {
		var e AllowlistEntry
		if err := rows.Scan(&e.MCUUID, &e.Username, &e.AddedAt, &e.CanWake); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *PGRepo) SetAllowlistWake(ctx context.Context, name, mcUUID string, canWake bool) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE server_allowlist
		    SET revoked_at = CASE WHEN $3 THEN NULL ELSE COALESCE(revoked_at, now()) END
		  WHERE server_name = $1 AND mc_uuid = $2`, name, mcUUID, canWake)
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

func (p *PGRepo) RequestRetire(ctx context.Context, name string, deleteServer bool) (RetireState, error) {
	var st RetireState
	switch err := p.db.QueryRowContext(ctx,
		`UPDATE servers
		    SET retire_requested_at = COALESCE(retire_requested_at, now()),
		        retire_delete = retire_delete OR $2
		  WHERE name = $1 AND deleted_at IS NULL
		  RETURNING retire_requested_at, retire_delete`, name, deleteServer).Scan(&st.RequestedAt, &st.Delete); {
	case errors.Is(err, sql.ErrNoRows):
		return RetireState{}, ErrNotFound
	case err != nil:
		return RetireState{}, err
	}
	return st, nil
}

// CancelRetire locks the row so a racing staff deletion cannot be cancelled by the owner.
func (p *PGRepo) CancelRetire(ctx context.Context, name string, mayCancelDelete bool) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	var deleteServer bool
	switch err := tx.QueryRowContext(ctx,
		`SELECT retire_delete FROM servers WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`,
		name).Scan(&deleteServer); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	}
	if deleteServer && !mayCancelDelete {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE servers SET retire_requested_at = NULL, retire_delete = false WHERE name = $1`, name); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *PGRepo) UserByMCUUID(ctx context.Context, mcUUID string) (string, error) {
	var userID string
	switch err := p.db.QueryRowContext(ctx,
		`SELECT al.user_id FROM account_links al
		   JOIN users u ON u.id = al.user_id
		  WHERE al.mc_uuid = $1 AND u.disabled = false AND u.deleted_at IS NULL`,
		mcUUID).Scan(&userID); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", err
	}
	return userID, nil
}

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
		COALESCE(s.owner_id = $1, false) AS owned, (s.owner_id IS NULL AND s.retire_requested_at IS NULL) AS claimable,
		COALESCE(s.cached_phase, ''), s.retire_requested_at, s.retire_delete
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
		var retireAt sql.NullTime
		var retireDelete bool
		if err := rows.Scan(&v.Name, &v.Subdomain, &v.Owned, &v.Claimable, &v.Phase, &retireAt, &retireDelete); err != nil {
			return nil, err
		}
		if v.Owned {
			v.Retiring = retireState(retireAt, retireDelete)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (p *PGRepo) ServerOwners(ctx context.Context) (map[string]ServerOwnership, error) {
	const q = `SELECT s.name, COALESCE(s.owner_id, ''), COALESCE(NULLIF(u.email, ''), u.username, ''),
		s.retire_requested_at, s.retire_delete
		FROM servers s LEFT JOIN users u ON u.id = s.owner_id
		WHERE s.deleted_at IS NULL`
	rows, err := p.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]ServerOwnership)
	for rows.Next() {
		var name string
		var o ServerOwnership
		var retireAt sql.NullTime
		var retireDelete bool
		if err := rows.Scan(&name, &o.OwnerID, &o.Owner, &retireAt, &retireDelete); err != nil {
			return nil, err
		}
		o.Retire = retireState(retireAt, retireDelete)
		out[name] = o
	}
	return out, rows.Err()
}

func (p *PGRepo) ServerNoticeRecipients(ctx context.Context, server string) ([]string, error) {
	var owner sql.NullString
	err := p.db.QueryRowContext(ctx, `SELECT owner_id FROM servers WHERE name = $1 AND deleted_at IS NULL`, server).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	const active = `email_verified AND COALESCE(email, '') <> '' AND NOT disabled AND deleted_at IS NULL`
	q, args := `SELECT email FROM users WHERE role = 'owner' AND `+active+` ORDER BY email`, []any{}
	if owner.String != "" {
		q, args = `SELECT email FROM users WHERE id = $1 AND `+active, []any{owner.String}
	}
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		out = append(out, email)
	}
	return out, rows.Err()
}

// SeedServer may leave a losing alias when same-name creates race across the database and cluster writes.
func (p *PGRepo) SeedServer(ctx context.Context, name, subdomain string, cpuMilli, memoryMB, storageMB int) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (name) DO UPDATE SET owner_id = NULL, claimed_at = NULL, last_active_at = now(),
		   warned_3d_at = NULL, warned_1d_at = NULL, cached_phase = NULL, created_at = now(), deleted_at = NULL,
		   retire_requested_at = NULL, retire_delete = false,
		   cached_cpu_milli = EXCLUDED.cached_cpu_milli, cached_memory_mb = EXCLUDED.cached_memory_mb,
		   cached_storage_mb = EXCLUDED.cached_storage_mb`,
		name, cpuMilli, memoryMB, storageMB); err != nil {
		return fmt.Errorf("seed server row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_allowlist WHERE server_name = $1`, name); err != nil {
		return fmt.Errorf("clear an earlier allowlist: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_aliases WHERE server_name = $1`, name); err != nil {
		return fmt.Errorf("clear earlier aliases: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM server_schedules WHERE server_name = $1`, name); err != nil {
		return fmt.Errorf("clear earlier schedules: %w", err)
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

func (p *PGRepo) AllBackups(ctx context.Context, opts BackupListOpts) ([]BackupView, int, error) {
	return p.pageBackups(ctx, ``, opts)
}

func (p *PGRepo) BackupsForUser(ctx context.Context, userID string, opts BackupListOpts) ([]BackupView, int, error) {
	return p.pageBackups(ctx, ` AND former_owner = $2`, opts, userID)
}

// pageBackups pages present backups under scope, whose parameters follow $1; id breaks created_at ties.
func (p *PGRepo) pageBackups(ctx context.Context, scope string, opts BackupListOpts, scopeArgs ...any) ([]BackupView, int, error) {
	match := ` FROM world_backups WHERE status = 'present' AND ($1::text = '' OR server_name = $1::text)` + scope
	args := append([]any{opts.Server}, scopeArgs...)
	var total int
	if err := p.db.QueryRowContext(ctx, `SELECT count(*)`+match, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(args)
	rows, err := p.db.QueryContext(ctx, fmt.Sprintf(`SELECT id, server_name, COALESCE(former_owner, ''), COALESCE(size_bytes, 0),
		reason, status, created_at, expires_at, corrupt_at IS NOT NULL, verified_at, skipped_entries%s
		ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, match, n+1, n+2), append(args, opts.Limit, opts.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanBackupViews(rows)
	return out, total, err
}

// scanBackupViews leaves backup_ref unselected: it never leaves the server.
func scanBackupViews(rows *sql.Rows) ([]BackupView, error) {
	defer rows.Close()
	var out []BackupView
	for rows.Next() {
		var v BackupView
		var verified sql.NullTime
		if err := rows.Scan(&v.ID, &v.ServerName, &v.FormerOwner, &v.SizeBytes,
			&v.Reason, &v.Status, &v.CreatedAt, &v.ExpiresAt, &v.Corrupt, &verified, &v.SkippedEntries); err != nil {
			return nil, err
		}
		if verified.Valid {
			v.VerifiedAt = &verified.Time
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (p *PGRepo) LatestBackup(ctx context.Context, serverName string) (*BackupRecord, error) {
	const q = `SELECT id, server_name, COALESCE(former_owner, ''), backup_ref, COALESCE(size_bytes, 0),
		COALESCE(sha256, ''), created_at
		FROM world_backups WHERE server_name = $1 AND status = 'present' AND corrupt_at IS NULL
		ORDER BY created_at DESC LIMIT 1`
	var b BackupRecord
	switch err := p.db.QueryRowContext(ctx, q, serverName).Scan(
		&b.ID, &b.ServerName, &b.FormerOwner, &b.BackupRef, &b.SizeBytes, &b.SHA256, &b.CreatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &b, nil
}

func (p *PGRepo) BackupByID(ctx context.Context, id string) (*BackupRecord, error) {
	const q = `SELECT id, server_name, COALESCE(former_owner, ''), backup_ref, COALESCE(size_bytes, 0),
		corrupt_at IS NOT NULL, COALESCE(sha256, '')
		FROM world_backups WHERE id = $1 AND status = 'present'`
	var b BackupRecord
	switch err := p.db.QueryRowContext(ctx, q, id).Scan(
		&b.ID, &b.ServerName, &b.FormerOwner, &b.BackupRef, &b.SizeBytes, &b.Corrupt, &b.SHA256); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &b, nil
}

func (p *PGRepo) ExpireBackup(ctx context.Context, id string, at time.Time) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE world_backups SET status = 'expired', expires_at = LEAST(expires_at, $2)
		 WHERE id = $1 AND status = 'present'`, id, at)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *PGRepo) LastBackupRequest(ctx context.Context, serverName string, since time.Time) (time.Time, error) {
	var at sql.NullTime
	err := p.db.QueryRowContext(ctx,
		`SELECT max(created_at) FROM audit_logs
		 WHERE created_at >= $2 AND action = 'backup.create' AND server_name = $1`,
		serverName, since).Scan(&at)
	if err != nil {
		return time.Time{}, err
	}
	return at.Time, nil
}

func (p *PGRepo) BackupStoreBytes(ctx context.Context) (int64, error) {
	var n int64
	err := p.db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(size_bytes), 0) FROM world_backups WHERE status = 'present'`).Scan(&n)
	return n, err
}

func (p *PGRepo) ScheduledBackupCandidates(ctx context.Context, before time.Time) ([]ScheduledCandidate, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT s.name, s.owner_id FROM servers s
		 LEFT JOIN LATERAL (
		   SELECT max(b.created_at) AS at FROM world_backups b
		   WHERE b.server_name = s.name AND b.reason = 'scheduled' AND b.status = 'present'
		     AND b.corrupt_at IS NULL AND b.former_owner = s.owner_id
		     AND b.created_at >= COALESCE(s.claimed_at, '-infinity')
		 ) pt ON true
		 WHERE s.deleted_at IS NULL AND s.owner_id IS NOT NULL
		   AND s.retire_requested_at IS NULL
		   AND s.last_active_at > COALESCE(pt.at, '-infinity')
		   AND COALESCE(pt.at, '-infinity') < $1
		 ORDER BY pt.at NULLS FIRST, s.name`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledCandidate
	for rows.Next() {
		var c ScheduledCandidate
		if err := rows.Scan(&c.Name, &c.OwnerID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *PGRepo) Audit(ctx context.Context, e AuditEntry) error {
	// A nil Payload is stored as SQL NULL, not the text "null".
	var payload any
	if len(e.Payload) > 0 {
		payload = string(e.Payload)
	}
	// actor_user_id resolves through users so a purged account lands as NULL instead of failing the foreign key.
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO audit_logs (actor, source, action, server_name, request_id, payload,
		                         actor_user_id, client_ip, user_agent)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6,
		         (SELECT id FROM users WHERE id = NULLIF($7, '')), NULLIF($8, '')::inet, NULLIF($9, ''))`,
		e.Actor, e.Source, e.Action, e.ServerName, e.RequestID, payload,
		e.ActorUserID, e.ClientIP, e.UserAgent)
	return err
}

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

func (p *PGRepo) AddLoginEmailOTP(ctx context.Context, id, userID, email, codeHash, purpose string, now, expiresAt time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM email_otps
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
		   AND id NOT IN (
		       SELECT id FROM email_otps
		       WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL AND expires_at > $3
		       ORDER BY created_at DESC, id DESC LIMIT $4)`,
		userID, purpose, now, otpLiveLoginCodes-1); err != nil {
		return fmt.Errorf("trim live login codes: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO email_otps (id, user_id, email, code_hash, purpose, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, userID, email, codeHash, purpose, expiresAt); err != nil {
		return fmt.Errorf("insert otp: %w", err)
	}
	return tx.Commit()
}

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
	if until, err := otpLockedUntil(ctx, tx, userID, purpose, now, true); err != nil {
		return "", err
	} else if !until.IsZero() {
		return "", &OTPAccountLockedError{Until: until}
	}
	if attempts >= otpMaxAttempts {
		return "", ErrOTPLocked
	}
	if storedHash != codeHash {
		return "", chargeOTPMismatch(ctx, tx, userID, purpose, now)
	}

	// users_verified_email_unique catches the cross-user race that passes this check.
	var taken bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users
		 WHERE lower(email) = lower($1) AND email_verified = true AND id <> $2)`,
		email, userID).Scan(&taken); err != nil {
		return "", fmt.Errorf("check verified-email uniqueness: %w", err)
	}
	if taken {
		return "", ErrEmailTaken
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE email_otps SET consumed_at = $2 WHERE id = $1`, id, now); err != nil {
		return "", fmt.Errorf("consume otp: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET email = $2, email_verified = true WHERE id = $1`, userID, email); err != nil {
		// The unique index rejected a concurrent verify; the rollback keeps the code live.
		if isUniqueViolation(err) {
			return "", ErrEmailTaken
		}
		return "", fmt.Errorf("mark email verified: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return email, nil
}

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

// ReclaimUsername preserves the original hold expiry on retry; empty dataRef stores NULL.
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

func (p *PGRepo) IsUsernameBlacklisted(ctx context.Context, mcUUID string) (bool, error) {
	var ok bool
	err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM username_blacklist WHERE mc_uuid = $1)`, mcUUID).Scan(&ok)
	return ok, err
}

func (p *PGRepo) IsProtectedAdminLink(ctx context.Context, mcUUID string) (bool, error) {
	var ok bool
	err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM account_links al JOIN users u ON u.id = al.user_id
			WHERE al.mc_uuid = $1 AND al.auth_source = 'thirdparty' AND u.role IN ('admin', 'owner'))`,
		mcUUID).Scan(&ok)
	return ok, err
}

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

func (p *PGRepo) AdminExists(ctx context.Context) (bool, error) {
	const q = `SELECT 1 FROM users WHERE role IN ('admin', 'owner') LIMIT 1`
	var one int
	switch err := p.db.QueryRowContext(ctx, q).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

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

func (p *PGRepo) UpsertOwner(ctx context.Context, id, username, email string) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO users (id, username, email, role) VALUES ($1, $2, NULLIF($3, ''), 'owner')
		 ON CONFLICT (username) DO UPDATE SET email = EXCLUDED.email, role = 'owner'`,
		id, username, email)
	return err
}

// OwnerUsername returns the single active owner or an empty string, preventing a second owner from being created.
func (p *PGRepo) OwnerUsername(ctx context.Context) (string, error) {
	var name string
	switch err := p.db.QueryRowContext(ctx,
		`SELECT username FROM users WHERE role = 'owner' AND deleted_at IS NULL ORDER BY created_at LIMIT 1`).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", err
	default:
		return name, nil
	}
}

func (p *PGRepo) InsertOperator(ctx context.Context, id, username, email string) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO users (id, username, email, role) VALUES ($1, $2, NULLIF($3, ''), 'admin')`,
		id, username, email)
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (p *PGRepo) CreateSession(ctx context.Context, s NewSession) error {
	return insertSession(ctx, p.db, s)
}

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertSession(ctx context.Context, db sqlExecer, s NewSession) error {
	reauth := sql.NullTime{Time: s.ReauthAt, Valid: !s.ReauthAt.IsZero()}
	_, err := db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, expires_at, user_agent, client_ip, reauth_at)
		 VALUES ($1, $2, $3, $3, $4, $5, $6, $7)`,
		s.TokenHash, s.UserID, s.CreatedAt, s.ExpiresAt, s.UserAgent, s.ClientIP, reauth)
	return err
}

// sessionLive uses $2 for now and $3 for the staff idle cutoff; disabled or deleted users never authenticate.
const sessionLive = `s.revoked_at IS NULL AND s.expires_at > $2
	AND u.disabled = false AND u.deleted_at IS NULL
	AND (u.role = 'user' OR s.last_seen_at > $3)`

func (p *PGRepo) SessionUser(ctx context.Context, tokenHash string, now time.Time) (*SessionedUser, error) {
	const q = `SELECT u.id, u.username, COALESCE(u.email, ''), u.role::text, COALESCE(u.email_verified, false),
			s.last_seen_at, s.reauth_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND ` + sessionLive
	var u SessionedUser
	var reauth sql.NullTime
	switch err := p.db.QueryRowContext(ctx, q, tokenHash, now, now.Add(-staffSessionIdle)).Scan(
		&u.ID, &u.Username, &u.Email, &u.Role, &u.EmailVerified, &u.LastSeenAt, &reauth); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	if reauth.Valid {
		u.ReauthAt = reauth.Time
	}
	return &u, nil
}

func (p *PGRepo) TouchSession(ctx context.Context, tokenHash string, now time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = $2 WHERE token_hash = $1 AND last_seen_at < $2`,
		tokenHash, now)
	return err
}

func (p *PGRepo) MarkSessionReauth(ctx context.Context, tokenHash string, at time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET reauth_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`,
		tokenHash, at)
	return err
}

func (p *PGRepo) RevokeSession(ctx context.Context, tokenHash string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`,
		tokenHash)
	return err
}

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

func (p *PGRepo) SetSetting(ctx context.Context, key string, value []byte) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO platform_settings (key, value) VALUES ($1, $2::jsonb)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, string(value))
	return err
}

func (p *PGRepo) CompareAndSetSetting(ctx context.Context, key string, expected, value []byte) error {
	query := `UPDATE platform_settings SET value = $2::jsonb, updated_at = now() WHERE key = $1 AND value = $3::jsonb`
	args := []any{key, string(value), string(expected)}
	if expected == nil {
		query = `INSERT INTO platform_settings (key, value) VALUES ($1, $2::jsonb) ON CONFLICT (key) DO NOTHING`
		args = args[:2]
	}
	res, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrConflict
	}
	return err
}

func (p *PGRepo) CreatePasskeyChallenge(ctx context.Context, id, userID, purpose string, sessionData []byte, expiresAt time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('passkey-challenge:' || $1 || ':' || $2))`,
		userID, purpose); err != nil {
		return fmt.Errorf("lock passkey challenges: %w", err)
	}
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

const maxLiveChallengesPerSource = 32 // live login challenges one source (IPv4 or IPv6 /48) may hold per store

// lockChallengeSource serializes a store's per-source count and insert; store names the table so stores do not block each other.
func lockChallengeSource(ctx context.Context, tx *sql.Tx, store, source string) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))`, store, source); err != nil {
		return fmt.Errorf("lock %s source: %w", store, err)
	}
	return nil
}

func (p *PGRepo) AddPasskeyLoginChallenge(ctx context.Context, id, userID, purpose, source, challenge string, sessionData []byte, now, expiresAt time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if err := lockChallengeSource(ctx, tx, "webauthn_challenges", source); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webauthn_challenges
		 WHERE user_id = $1 AND purpose = $2 AND (expires_at <= $3 OR consumed_at IS NOT NULL)`,
		userID, purpose, now); err != nil {
		return fmt.Errorf("reap login challenges: %w", err)
	}
	var fromSource int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM webauthn_challenges
		 WHERE source = $1 AND consumed_at IS NULL AND expires_at > $2`,
		source, now).Scan(&fromSource); err != nil {
		return fmt.Errorf("count login challenges: %w", err)
	}
	if fromSource >= maxLiveChallengesPerSource {
		return ErrTooManyPasskeyChallenges
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO webauthn_challenges (id, user_id, purpose, session_data, expires_at, challenge, source)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, userID, purpose, sessionData, expiresAt, challenge, source); err != nil {
		return fmt.Errorf("insert login challenge: %w", err)
	}
	return tx.Commit()
}

func (p *PGRepo) ConsumePasskeyLoginChallenge(ctx context.Context, userID, purpose, challenge string, now time.Time) ([]byte, error) {
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
		 WHERE user_id = $1 AND purpose = $2 AND challenge = $3 AND consumed_at IS NULL
		 FOR UPDATE`,
		userID, purpose, challenge).Scan(&id, &sessionData, &expiresAt); {
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
		return nil, fmt.Errorf("consume login challenge: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sessionData, nil
}

const maxLiveDiscoverableChallenges = 16384 // table-wide live-row bound for the discoverable-login store

// CreateDiscoverableChallenge enforces per-source bounds; concurrent sources may overshoot the table-wide bound.
func (p *PGRepo) CreateDiscoverableChallenge(ctx context.Context, id, source string, sessionData []byte, now, expiresAt time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if err := lockChallengeSource(ctx, tx, "webauthn_discoverable_challenges", source); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webauthn_discoverable_challenges WHERE expires_at <= $1 OR consumed_at IS NOT NULL`,
		now); err != nil {
		return fmt.Errorf("reap discoverable challenges: %w", err)
	}
	var live, fromSource int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*), count(*) FILTER (WHERE source = $1) FROM webauthn_discoverable_challenges`,
		source).Scan(&live, &fromSource); err != nil {
		return fmt.Errorf("count discoverable challenges: %w", err)
	}
	if fromSource >= maxLiveChallengesPerSource || live >= maxLiveDiscoverableChallenges {
		return ErrTooManyPasskeyChallenges
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO webauthn_discoverable_challenges (id, session_data, expires_at, source)
		 VALUES ($1, $2, $3, $4)`,
		id, sessionData, expiresAt, source); err != nil {
		return fmt.Errorf("insert discoverable challenge: %w", err)
	}
	return tx.Commit()
}

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

// AdvanceCredentialSignCount only raises counters; concurrent lower assertions cannot reopen replay and removed credentials are a no-op.
func (p *PGRepo) AdvanceCredentialSignCount(ctx context.Context, credentialID string, newSignCount uint32, usedAt time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE webauthn_credentials SET sign_count = GREATEST(sign_count, $2), last_used_at = $3
		  WHERE credential_id = $1`,
		credentialID, int64(newSignCount), usedAt)
	return err
}

func (p *PGRepo) DeletePasskeyCredential(ctx context.Context, userID, id string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// The user row lock queues deletes so the count below cannot go stale before the DELETE.
	var verified bool
	switch err := tx.QueryRowContext(ctx,
		`SELECT email_verified FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&verified); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	}
	var mine, total int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE id = $2), count(*) FROM webauthn_credentials WHERE user_id = $1`,
		userID, id).Scan(&mine, &total); err != nil {
		return err
	}
	if mine == 0 {
		return ErrNotFound
	}
	if total == 1 && !verified {
		return ErrLastPasskey
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE id = $1 AND user_id = $2`, id, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *PGRepo) DeleteAllPasskeyCredentialsForUser(ctx context.Context, userID string) error {
	_, err := p.db.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE user_id = $1`, userID)
	return err
}

func (p *PGRepo) ListUsers(ctx context.Context, opts ListUsersOpts) ([]UserView, int, error) {
	limit := opts.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}

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

	var total int
	if err := p.db.QueryRowContext(ctx, `SELECT count(*) FROM users u`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
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

	linkRows, err := p.db.QueryContext(ctx,
		`SELECT mc_uuid::text, COALESCE(auth_source, 'mojang'), verified_at
		 FROM account_links WHERE user_id = $1 ORDER BY verified_at`, userID)
	if err != nil {
		return nil, err
	}
	defer linkRows.Close()
	for linkRows.Next() {
		var a LinkedAccount
		if err := linkRows.Scan(&a.MCUUID, &a.AuthSource, &a.VerifiedAt); err != nil {
			return nil, err
		}
		d.LinkedAccounts = append(d.LinkedAccounts, a)
	}
	if err := linkRows.Err(); err != nil {
		return nil, err
	}
	return &d, nil
}

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

func (p *PGRepo) UpdateUser(ctx context.Context, userID string, patch UpdateUserInput, _ string) (*UserView, error) {
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
		// A changed address drops email_verified; only VerifyEmailOTP may assert it.
		sets = append(sets,
			fmt.Sprintf("email = NULLIF($%d, '')", argn),
			fmt.Sprintf("email_verified = (email_verified AND email IS NOT DISTINCT FROM NULLIF($%d, ''))", argn))
		args = append(args, *patch.Email)
	}
	if patch.Role != nil {
		argn++
		sets = append(sets, fmt.Sprintf("role = $%d::user_role", argn))
		args = append(args, *patch.Role)
	}
	if len(sets) == 0 {
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
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return &v, nil
}

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

func (p *PGRepo) DeleteUser(ctx context.Context, userID, _ string, clearPorts func(context.Context, string) error) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, userID); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		userID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM servers WHERE owner_id = $1 AND deleted_at IS NULL ORDER BY name FOR UPDATE`, userID)
	if err != nil {
		return err
	}
	var servers []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		servers = append(servers, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, name := range servers {
		if err := clearPorts(ctx, name); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`WITH released AS (
		   UPDATE servers SET owner_id = NULL WHERE owner_id = $1 AND deleted_at IS NULL RETURNING name)
		 DELETE FROM server_allowlist WHERE server_name IN (SELECT name FROM released)`,
		userID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_links WHERE user_id = $1`, userID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET disabled = true, deleted_at = now() WHERE id = $1`,
		userID); err != nil {
		return err
	}

	return tx.Commit()
}

func (p *PGRepo) PatchServerPorts(ctx context.Context, name, ownerID string, apply func() error) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(owner_id, '') FROM servers WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`, name).Scan(&owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if ownerID != "" && owner != ownerID {
		return errForbidden
	}
	if err := apply(); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *PGRepo) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE users SET disabled = $2 WHERE id = $1 AND deleted_at IS NULL`, userID, disabled)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}

	if disabled {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
			userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p *PGRepo) GetQuotas(ctx context.Context, userID string) (*QuotaView, error) {
	if err := p.requireLiveUser(ctx, userID); err != nil {
		return nil, err
	}
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
	v.Custom = true
	return &v, nil
}

func (p *PGRepo) requireLiveUser(ctx context.Context, userID string) error {
	var ok bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		userID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

func (p *PGRepo) SetQuotas(ctx context.Context, userID string, qi QuotaInput, setBy string) (*QuotaView, error) {
	if err := p.requireLiveUser(ctx, userID); err != nil {
		return nil, err
	}
	v := QuotaView{}
	if err := p.db.QueryRowContext(ctx,
		`INSERT INTO quotas (user_id, updated_by, max_servers, max_cpu_milli, max_memory_mb, max_storage_gb)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (user_id) DO UPDATE SET updated_by = EXCLUDED.updated_by,
		   max_servers = EXCLUDED.max_servers, max_cpu_milli = EXCLUDED.max_cpu_milli,
		   max_memory_mb = EXCLUDED.max_memory_mb, max_storage_gb = EXCLUDED.max_storage_gb
		 RETURNING user_id, max_servers, max_cpu_milli, max_memory_mb, max_storage_gb`,
		userID, setBy, qi.MaxServers, qi.MaxCPUMilli, qi.MaxMemoryMB, qi.MaxStorageGB).Scan(
		&v.UserID, &v.MaxServers, &v.MaxCPUMilli, &v.MaxMemoryMB, &v.MaxStorageGB); err != nil {
		return nil, err
	}
	v.Custom = true
	return &v, nil
}

func (p *PGRepo) ClearQuotas(ctx context.Context, userID string) (*QuotaView, error) {
	if err := p.requireLiveUser(ctx, userID); err != nil {
		return nil, err
	}
	if _, err := p.db.ExecContext(ctx, `DELETE FROM quotas WHERE user_id = $1`, userID); err != nil {
		return nil, err
	}
	return &QuotaView{UserID: userID}, nil
}

func (p *PGRepo) ListUserSessions(ctx context.Context, userID string, now time.Time) ([]SessionView, error) {
	const q = `SELECT s.token_hash, s.created_at, s.expires_at, s.last_seen_at, s.user_agent, s.client_ip
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.user_id = $1 AND ` + sessionLive + `
		ORDER BY s.last_seen_at DESC, s.created_at DESC`
	rows, err := p.db.QueryContext(ctx, q, userID, now, now.Add(-staffSessionIdle))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionView
	for rows.Next() {
		var s SessionView
		if err := rows.Scan(&s.TokenHash, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt, &s.UserAgent, &s.ClientIP); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *PGRepo) RevokeAllUserSessions(ctx context.Context, userID string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID)
	return err
}

func (p *PGRepo) RevokeUserSession(ctx context.Context, userID, tokenHash string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now()
		 WHERE token_hash = $1 AND user_id = $2 AND revoked_at IS NULL AND expires_at > now()`,
		tokenHash, userID)
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

func (p *PGRepo) RevokeOtherUserSessions(ctx context.Context, userID, keepTokenHash string) (int, error) {
	res, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now()
		 WHERE user_id = $1 AND token_hash <> $2 AND revoked_at IS NULL AND expires_at > now()`,
		userID, keepTokenHash)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

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

func (p *PGRepo) LinkAccount(ctx context.Context, userID, mcUUID, authSource string) error {
	if err := p.requireLiveUser(ctx, userID); err != nil {
		return err
	}
	var exists bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM account_links WHERE user_id = $1 AND mc_uuid = $2)`,
		userID, mcUUID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}

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

// StartMigration checks liveness on the INSERT's fresh snapshot after superseding, so a racing redeem's retirement is visible.
func (p *PGRepo) StartMigration(ctx context.Context, id, sourceUserID string, now time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('account-migration:' || $1))`, sourceUserID); err != nil {
		return fmt.Errorf("lock migration source: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM account_migrations WHERE source_user_id = $1 AND state <> 'redeemed'`,
		sourceUserID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO account_migrations (id, source_user_id, state, created_at, updated_at)
		 SELECT $1, $2, 'initiated', $3, $3
		 WHERE EXISTS (SELECT 1 FROM users WHERE id = $2 AND deleted_at IS NULL)`,
		id, sourceUserID, now)
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
	return tx.Commit()
}

func (p *PGRepo) MigrationForSource(ctx context.Context, sourceUserID string) (*MigrationView, error) {
	const q = `SELECT id, source_user_id, COALESCE(target_user_id, ''), state,
		COALESCE(confirm_factor, ''), COALESCE(confirm_session, ''), confirmed_at,
		code_expires_at, created_at
		FROM account_migrations
		WHERE source_user_id = $1 AND state <> 'redeemed'`
	var v MigrationView
	switch err := p.db.QueryRowContext(ctx, q, sourceUserID).Scan(
		&v.ID, &v.SourceUserID, &v.TargetUserID, &v.State,
		&v.ConfirmFactor, &v.ConfirmSession, &v.ConfirmedAt, &v.CodeExpiresAt, &v.CreatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &v, nil
}

// ConfirmMigration clears expired codes and accepts only states where this session must repeat fresh proof.
func (p *PGRepo) ConfirmMigration(ctx context.Context, sourceUserID, factor, session string, now time.Time) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE account_migrations
		 SET state = 'confirmed', confirm_factor = $2, confirmed_at = $3, confirm_session = $4,
		     target_user_id = NULL, code_hash = NULL, code_expires_at = NULL
		 WHERE source_user_id = $1 AND (
		   state = 'initiated'
		   OR (state = 'confirmed' AND (confirm_session IS DISTINCT FROM $4 OR confirmed_at <= $5))
		   OR (state = 'code_issued' AND code_expires_at <= $3))`,
		sourceUserID, factor, now, session, now.Add(-migrateConfirmWindow))
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

// IssueMigrationCode requires fresh same-session proof; the caller validates the target and the foreign key is a final backstop.
func (p *PGRepo) IssueMigrationCode(ctx context.Context, sourceUserID, targetUserID, session, codeHash string, now, expiresAt time.Time) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE account_migrations
		 SET state = 'code_issued', target_user_id = $2, code_hash = $3, code_expires_at = $4
		 WHERE source_user_id = $1 AND state = 'confirmed'
		   AND confirm_session = $5 AND confirmed_at > $6`,
		sourceUserID, targetUserID, codeHash, expiresAt, session, now.Add(-migrateConfirmWindow))
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

func (p *PGRepo) RedeemMigration(ctx context.Context, targetUserID, codeHash string, now time.Time) (string, []string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// Lock only the pending migration bound to this target so intercepted codes cannot redeem for another account.
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

	// Lock both claim lanes in account-ID order to prevent crossing migrations from deadlocking and quota checks from racing claims.
	lanes := []string{sourceUserID, targetUserID}
	if lanes[1] < lanes[0] {
		lanes[0], lanes[1] = lanes[1], lanes[0]
	}
	for _, id := range lanes {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, id); err != nil {
			return "", nil, err
		}
	}

	// Quota failure rolls back without consuming the code; an empty source may retire even when the target is already over quota.
	var n, cpu, mem, stor int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cached_cpu_milli), 0), COALESCE(SUM(cached_memory_mb), 0), COALESCE(SUM(cached_storage_mb), 0)
		 FROM servers WHERE owner_id = $1 AND deleted_at IS NULL`,
		sourceUserID).Scan(&n, &cpu, &mem, &stor); err != nil {
		return "", nil, err
	}
	if n > 0 {
		caps, err := readQuota(ctx, tx, targetUserID)
		if err != nil {
			return "", nil, err
		}
		maxServers, maxCPU, maxMem, maxStor := caps.MaxServers, caps.MaxCPU, caps.MaxMem, caps.MaxStor
		var count, cpuSum, memSum, storSum int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(cached_cpu_milli), 0), COALESCE(SUM(cached_memory_mb), 0), COALESCE(SUM(cached_storage_mb), 0)
			 FROM servers WHERE owner_id = $1 AND deleted_at IS NULL`,
			targetUserID).Scan(&count, &cpuSum, &memSum, &storSum); err != nil {
			return "", nil, err
		}
		if !quotaAllows(maxServers, maxCPU, maxMem, maxStor, count, cpuSum, memSum, storSum,
			n, ResourceSpec{CPUMilli: int(cpu), MemoryMB: int(mem), StorageMB: int(stor)}) {
			return "", nil, ErrQuotaExceeded
		}
	}

	rows, err := tx.QueryContext(ctx,
		`UPDATE servers SET owner_id = $2, claimed_at = $3
		 WHERE owner_id = $1 AND deleted_at IS NULL
		 RETURNING name`,
		sourceUserID, targetUserID, now)
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

	if _, err := tx.ExecContext(ctx,
		`UPDATE server_schedules s SET owner_id = $2 FROM servers v
		 WHERE v.name = s.server_name AND v.owner_id = $2 AND s.owner_id = $1`,
		sourceUserID, targetUserID); err != nil {
		return "", nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`,
		sourceUserID, now); err != nil {
		return "", nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET disabled = true, deleted_at = $2 WHERE id = $1 AND deleted_at IS NULL`,
		sourceUserID, now); err != nil {
		return "", nil, err
	}

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

func (p *PGRepo) UserByEmail(ctx context.Context, email string) (*StaffUser, error) {
	// Case-insensitive verified-email lookup excludes disabled and deleted accounts to prevent sign-in after retirement.
	const q = `SELECT id, username, COALESCE(email, ''), role::text, email_verified
		FROM users WHERE lower(email) = lower($1) AND email_verified = true
		  AND disabled = false AND deleted_at IS NULL`
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

// ConsumeLoginEmailOTP locks all live codes, charges mismatches to every eligible code and the account, and consumes all on a match.
func (p *PGRepo) ConsumeLoginEmailOTP(ctx context.Context, userID, purpose, codeHash string, now time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	rows, err := tx.QueryContext(ctx,
		`SELECT code_hash, attempts FROM email_otps
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL AND expires_at > $3
		 FOR UPDATE`,
		userID, purpose, now)
	if err != nil {
		return err
	}
	var live, open int
	matched := false
	for rows.Next() {
		var (
			storedHash string
			attempts   int
		)
		if err := rows.Scan(&storedHash, &attempts); err != nil {
			rows.Close()
			return err
		}
		live++
		if attempts < otpMaxAttempts {
			open++
			matched = matched || storedHash == codeHash
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if live == 0 {
		return ErrOTPInvalid
	}
	if until, err := otpLockedUntil(ctx, tx, userID, purpose, now, true); err != nil {
		return err
	} else if !until.IsZero() {
		return &OTPAccountLockedError{Until: until}
	}
	if open == 0 {
		return ErrOTPLocked
	}
	if !matched {
		return chargeOTPMismatch(ctx, tx, userID, purpose, now)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE email_otps SET consumed_at = $3
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL`,
		userID, purpose, now); err != nil {
		return fmt.Errorf("consume otp: %w", err)
	}
	return tx.Commit()
}

func (p *PGRepo) OTPLockedUntil(ctx context.Context, userID, purpose string, now time.Time) (time.Time, error) {
	return otpLockedUntil(ctx, p.db, userID, purpose, now, false)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// otpLockedUntil optionally locks the budget row so checking and charging serialize.
func otpLockedUntil(ctx context.Context, q rowQuerier, userID, purpose string, now time.Time, forUpdate bool) (time.Time, error) {
	query := `SELECT window_start, failures FROM otp_failure_windows WHERE user_id = $1 AND purpose = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var (
		start    time.Time
		failures int
	)
	switch err := q.QueryRowContext(ctx, query, userID, purpose).Scan(&start, &failures); {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, fmt.Errorf("read otp failure budget: %w", err)
	}
	return otpLockEnd(start, failures, now), nil
}

func otpLockEnd(windowStart time.Time, failures int, now time.Time) time.Time {
	end := windowStart.Add(otpFailureWindow)
	if failures < otpFailureBudget || !end.After(now) {
		return time.Time{}
	}
	return end
}

// chargeOTPMismatch requires the compared codes locked FOR UPDATE and commits the failed attempt before returning its refusal.
func chargeOTPMismatch(ctx context.Context, tx *sql.Tx, userID, purpose string, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE email_otps SET attempts = attempts + 1
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
		   AND expires_at > $3 AND attempts < $4`,
		userID, purpose, now, otpMaxAttempts); err != nil {
		return fmt.Errorf("record otp attempt: %w", err)
	}
	var (
		start    time.Time
		failures int
	)
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO otp_failure_windows (user_id, purpose, window_start, failures)
		 VALUES ($1, $2, $3, 1)
		 ON CONFLICT (user_id, purpose) DO UPDATE SET
		   failures = CASE WHEN otp_failure_windows.window_start + $4 * interval '1 second' <= $3
		                   THEN 1 ELSE otp_failure_windows.failures + 1 END,
		   window_start = CASE WHEN otp_failure_windows.window_start + $4 * interval '1 second' <= $3
		                   THEN $3 ELSE otp_failure_windows.window_start END
		 RETURNING window_start, failures`,
		userID, purpose, now, int64(otpFailureWindow/time.Second)).Scan(&start, &failures); err != nil {
		return fmt.Errorf("charge otp failure budget: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if failures == otpFailureBudget {
		return &OTPAccountLockedError{Until: start.Add(otpFailureWindow), JustLocked: true}
	}
	return ErrOTPInvalid
}

func (p *PGRepo) CreateOpLoginRequest(ctx context.Context, req NewOpLoginRequest) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO op_login_requests (id, user_id, email, expires_at, client_ip, user_agent)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		req.ID, req.UserID, req.Email, req.ExpiresAt, req.ClientIP, req.UserAgent)
	return err
}

func (p *PGRepo) OpLoginRequestByID(ctx context.Context, id string) (*OpLoginRequest, error) {
	const q = `SELECT r.id, r.user_id, u.username, r.email, r.expires_at, r.created_at,
		       r.consumed_at, r.approved_at, r.client_ip, r.user_agent
		FROM op_login_requests r JOIN users u ON u.id = r.user_id WHERE r.id = $1`
	var (
		r          OpLoginRequest
		consumedAt sql.NullTime
		approvedAt sql.NullTime
	)
	switch err := p.db.QueryRowContext(ctx, q, id).Scan(
		&r.ID, &r.UserID, &r.Username, &r.Email, &r.ExpiresAt, &r.CreatedAt,
		&consumedAt, &approvedAt, &r.ClientIP, &r.UserAgent); {
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

func (p *PGRepo) ListPendingOpLogins(ctx context.Context, now time.Time) ([]OpLoginRequest, error) {
	const q = `SELECT r.id, r.user_id, u.username, r.email, r.expires_at, r.created_at,
		       r.client_ip, r.user_agent
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
		if err := rows.Scan(&r.ID, &r.UserID, &r.Username, &r.Email, &r.ExpiresAt, &r.CreatedAt,
			&r.ClientIP, &r.UserAgent); err != nil {
			return nil, err
		}
		r.Status = "pending"
		out = append(out, r)
	}
	return out, rows.Err()
}

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

func (p *PGRepo) ConsumeOpLoginRequest(ctx context.Context, id string, now time.Time) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE op_login_requests SET consumed_at = $2
		 WHERE id = $1 AND approved_at IS NOT NULL AND consumed_at IS NULL AND expires_at > $2`,
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

func (p *PGRepo) RedeemSetupToken(ctx context.Context, tokenHash string, now time.Time, s NewSession) (string, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var userID string
	switch err := tx.QueryRowContext(ctx,
		`UPDATE setup_tokens SET consumed_at = $2
		 WHERE token_hash = $1 AND consumed_at IS NULL AND expires_at > $2
		 RETURNING user_id`,
		tokenHash, now).Scan(&userID); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", err
	}
	s.UserID = userID
	if err := insertSession(ctx, tx, s); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return userID, nil
}

// CreateSetupToken stores only the hash; initial setup uses CompleteOwnerSetup to create owner, auth setting and token atomically.
func (p *PGRepo) CreateSetupToken(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO setup_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt)
	return err
}

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
