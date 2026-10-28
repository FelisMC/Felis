package api

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const apiTokenLive = `t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > $2)`

const apiTokenColumns = `t.id, t.user_id, t.name, t.prefix, t.admin, t.created_at, t.expires_at, t.last_used_at`

func scanAPIToken(row rowScanner, extra ...any) (*APIToken, error) {
	var t APIToken
	var expires, used sql.NullTime
	dest := []any{&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Admin, &t.CreatedAt, &expires, &used}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	t.ExpiresAt, t.LastUsedAt = nullTimePtr(expires), nullTimePtr(used)
	return &t, nil
}

// CreateAPIToken locks the user row so concurrent creates cannot exceed the limit.
func (p *PGRepo) CreateAPIToken(ctx context.Context, t *APIToken, hash string, limit int, now time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var one int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE id = $1 AND disabled = false AND deleted_at IS NULL FOR UPDATE`, t.UserID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM api_tokens t WHERE t.user_id = $1 AND `+apiTokenLive, t.UserID, now).Scan(&n); err != nil {
		return err
	}
	if n >= limit {
		return ErrAPITokenLimit
	}
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO api_tokens (id, user_id, name, prefix, token_hash, admin, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
		t.ID, t.UserID, t.Name, t.Prefix, hash, t.Admin, now, t.ExpiresAt).Scan(&t.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *PGRepo) ListAPITokens(ctx context.Context, userID string, now time.Time) ([]APIToken, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens t WHERE t.user_id = $1 AND `+apiTokenLive+`
		 ORDER BY t.created_at DESC, t.id`, userID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (p *PGRepo) RevokeAPIToken(ctx context.Context, userID, id string, now time.Time) error {
	ok, err := p.execApplied(ctx,
		`UPDATE api_tokens SET revoked_at = $3 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, id, userID, now)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

func (p *PGRepo) APITokenUser(ctx context.Context, hash string, now time.Time) (*StaffUser, *APIToken, error) {
	var u StaffUser
	t, err := scanAPIToken(p.db.QueryRowContext(ctx,
		`SELECT `+apiTokenColumns+`, u.id, u.username, COALESCE(u.email, ''), u.role::text, COALESCE(u.email_verified, false)
		 FROM api_tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.token_hash = $1 AND `+apiTokenLive+` AND u.disabled = false AND u.deleted_at IS NULL`, hash, now),
		&u.ID, &u.Username, &u.Email, &u.Role, &u.EmailVerified)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil, ErrNotFound
	case err != nil:
		return nil, nil, err
	}
	return &u, t, nil
}

func (p *PGRepo) TouchAPIToken(ctx context.Context, id string, now time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = $2 WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2)`, id, now)
	return err
}
