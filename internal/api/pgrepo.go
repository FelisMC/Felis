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
		`SELECT mc_uuid, auth_source FROM account_link_codes WHERE code = $1 AND expires_at > $2`,
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
// linked user is an admin. It intentionally does not test password_hash: an Operator
// who signs in via SSO (Cloudflare Access, §14) carries role='admin' with a NULL hash
// and must be protected just the same — the hash is orthogonal to "is staff" and "logs
// in via the Login Server". Keyed by UUID, the only identity velocity holds.
func (p *PGRepo) IsProtectedAdminLink(ctx context.Context, mcUUID string) (bool, error) {
	var ok bool
	err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM account_links al JOIN users u ON u.id = al.user_id
			WHERE al.mc_uuid = $1 AND al.auth_source = 'thirdparty' AND u.role = 'admin')`,
		mcUUID).Scan(&ok)
	return ok, err
}

// ---- local-password auth (spec §B) ----

// UserByUsername loads a staff login projection by username, or ErrNotFound. A
// player row (NULL password_hash) is returned with an empty PasswordHash, never
// hidden — the caller rejects it by the hash compare, so login cannot be used to
// enumerate which usernames carry a password.
func (p *PGRepo) UserByUsername(ctx context.Context, username string) (*StaffUser, error) {
	const q = `SELECT id, username, COALESCE(email, ''), role::text,
		COALESCE(password_hash, ''), must_change_password, email_verified
		FROM users WHERE username = $1`
	var u StaffUser
	switch err := p.db.QueryRowContext(ctx, q, username).Scan(
		&u.ID, &u.Username, &u.Email, &u.Role, &u.PasswordHash, &u.MustChangePassword, &u.EmailVerified); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &u, nil
}

// AdminExists reports whether any authenticatable staff account already exists —
// an admin row WITH a bcrypt password hash. It is the break-glass console's
// bootstrap-vs-recovery switch: false means the typed credential mints the first
// Owner (no prior identity to verify against), true means the operator must
// identify against an existing admin for accountability. It is not on the Repo
// interface because only the break-glass CLI consults it.
func (p *PGRepo) AdminExists(ctx context.Context) (bool, error) {
	const q = `SELECT EXISTS (
		SELECT 1 FROM users WHERE role = 'admin' AND password_hash IS NOT NULL)`
	var exists bool
	if err := p.db.QueryRowContext(ctx, q).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// UserByID loads the same staff projection by id, or ErrNotFound. The
// change-password flow re-verifies the caller's current password with it: the
// session yields a user id, not a username.
func (p *PGRepo) UserByID(ctx context.Context, id string) (*StaffUser, error) {
	const q = `SELECT id, username, COALESCE(email, ''), role::text,
		COALESCE(password_hash, ''), must_change_password, email_verified
		FROM users WHERE id = $1`
	var u StaffUser
	switch err := p.db.QueryRowContext(ctx, q, id).Scan(
		&u.ID, &u.Username, &u.Email, &u.Role, &u.PasswordHash, &u.MustChangePassword, &u.EmailVerified); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &u, nil
}

// UpsertOwner creates or resets the Owner account direct-to-Postgres (the
// break-glass first-run / reset-password path). role is forced to 'admin'; on a
// username conflict the email, hash and must_change_password flag are overwritten
// while the existing id is preserved, so live sessions referencing it survive a
// password reset. The empty email is stored as NULL (users.email is nullable).
func (p *PGRepo) UpsertOwner(ctx context.Context, id, username, email, passwordHash string, mustChange bool) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO users (id, username, email, role, password_hash, must_change_password)
		 VALUES ($1, $2, NULLIF($3, ''), 'admin', $4, $5)
		 ON CONFLICT (username) DO UPDATE SET
		   email = NULLIF($3, ''), role = 'admin',
		   password_hash = $4, must_change_password = $5`,
		id, username, email, passwordHash, mustChange)
	return err
}

// InsertOperator mints a NEW Operator (additional staff admin) account
// direct-to-Postgres. role is forced to 'admin' — Felis has no separate operator
// role, so an Operator is an additional admin row identical in shape to the Owner
// (migration 0003). UNLIKE UpsertOwner this is insert-only: a username conflict is
// left untouched (ON CONFLICT DO NOTHING) and reported as ErrConflict via a zero
// RowsAffected, so adding an Operator can never silently reset the Owner's or
// another Operator's credential. The empty email is stored as NULL.
func (p *PGRepo) InsertOperator(ctx context.Context, id, username, email, passwordHash string, mustChange bool) error {
	res, err := p.db.ExecContext(ctx,
		`INSERT INTO users (id, username, email, role, password_hash, must_change_password)
		 VALUES ($1, $2, NULLIF($3, ''), 'admin', $4, $5)
		 ON CONFLICT (username) DO NOTHING`,
		id, username, email, passwordHash, mustChange)
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

// SetPassword stores a new hash and clears must_change_password (the panel
// change-password flow). ErrNotFound when no row matches so a stale session
// cannot silently no-op the change.
func (p *PGRepo) SetPassword(ctx context.Context, userID, passwordHash string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE users SET password_hash = $2, must_change_password = false WHERE id = $1`,
		userID, passwordHash)
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
	const q = `SELECT u.id, COALESCE(u.email, ''), u.role::text, u.must_change_password
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > $2`
	var u SessionedUser
	switch err := p.db.QueryRowContext(ctx, q, tokenHash, now).Scan(
		&u.ID, &u.Email, &u.Role, &u.MustChangePassword); {
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

// RevokeUserSessionsExcept revokes every live session of a user except
// keepTokenHash — the change-password flow logs out the account's other devices
// while keeping the current one.
func (p *PGRepo) RevokeUserSessionsExcept(ctx context.Context, userID, keepTokenHash string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = now()
		 WHERE user_id = $1 AND token_hash <> $2 AND revoked_at IS NULL`,
		userID, keepTokenHash)
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

// CreatePasskeyCredential stores a freshly verified passkey (enrollment). Only public
// attestation material is written; a credential_id already bound to ANY account is left
// untouched (ON CONFLICT DO NOTHING) and reported as ErrConflict via a zero RowsAffected,
// so an authenticator is never silently rebound. Empty aaguid/name land as SQL NULL.
func (p *PGRepo) CreatePasskeyCredential(ctx context.Context, c PasskeyCredential) error {
	res, err := p.db.ExecContext(ctx,
		`INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, sign_count, aaguid, name, created_at)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8)
		 ON CONFLICT (credential_id) DO NOTHING`,
		c.ID, c.UserID, c.CredentialID, c.PublicKey, int64(c.SignCount), c.AAGUID, c.Name, c.CreatedAt)
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
		COALESCE(aaguid, ''), COALESCE(name, ''), created_at, last_used_at
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
			&c.AAGUID, &c.Name, &c.CreatedAt, &lastUsed); err != nil {
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
