package api

import (
	"context"
	"database/sql"
	"errors"
)

func (p *PGRepo) SetServerProtected(ctx context.Context, name string, protected bool) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE servers SET protected = $2 WHERE name = $1 AND deleted_at IS NULL`,
		name, protected)
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

func (p *PGRepo) ServerCollaborators(ctx context.Context, name string) ([]CollaboratorRecord, error) {
	const q = `SELECT sc.server_name, sc.user_id, u.username, COALESCE(u.email, ''), u.role::text,
		sc.role, COALESCE(sc.granted_by, ''), sc.created_at, sc.updated_at
		FROM server_collaborators sc
		JOIN users u ON u.id = sc.user_id
		WHERE sc.server_name = $1 AND u.deleted_at IS NULL
		ORDER BY sc.created_at ASC, u.username ASC`
	rows, err := p.db.QueryContext(ctx, q, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollaboratorRecord{}
	for rows.Next() {
		var r CollaboratorRecord
		if err := rows.Scan(&r.ServerName, &r.UserID, &r.Username, &r.Email, &r.UserRole,
			&r.Role, &r.GrantedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *PGRepo) CollaboratorCandidates(ctx context.Context, limit int) ([]CollaboratorCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `SELECT id, username, COALESCE(email, ''), role::text
		FROM users
		WHERE deleted_at IS NULL AND NOT disabled AND role <> 'owner'
		ORDER BY (role = 'admin') DESC, username ASC
		LIMIT $1`
	rows, err := p.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollaboratorCandidate{}
	for rows.Next() {
		var c CollaboratorCandidate
		if err := rows.Scan(&c.ID, &c.Username, &c.Email, &c.Role); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (p *PGRepo) UserCollaboratorRole(ctx context.Context, name, userID string) (string, error) {
	var role string
	err := p.db.QueryRowContext(ctx,
		`SELECT sc.role FROM server_collaborators sc
		 JOIN servers s ON s.name = sc.server_name
		 WHERE sc.server_name = $1 AND sc.user_id = $2 AND s.deleted_at IS NULL`,
		name, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func (p *PGRepo) UserCollaboratorRoles(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT sc.server_name, sc.role FROM server_collaborators sc
		 JOIN servers s ON s.name = sc.server_name
		 WHERE sc.user_id = $1 AND s.deleted_at IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, role string
		if err := rows.Scan(&name, &role); err != nil {
			return nil, err
		}
		out[name] = role
	}
	return out, rows.Err()
}

func (p *PGRepo) HasAnyCollaboratorAccess(ctx context.Context, userID string) (bool, error) {
	var ok bool
	err := p.db.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM server_collaborators sc
			JOIN servers s ON s.name = sc.server_name
			WHERE sc.user_id = $1 AND s.deleted_at IS NULL
		)`, userID).Scan(&ok)
	return ok, err
}

func (p *PGRepo) UpsertServerCollaborator(ctx context.Context, name, userID, role, grantedBy string) (*CollaboratorRecord, error) {
	const q = `INSERT INTO server_collaborators (server_name, user_id, role, granted_by)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (server_name, user_id)
		DO UPDATE SET role = EXCLUDED.role, granted_by = EXCLUDED.granted_by, updated_at = now()
		RETURNING server_name, user_id, role, COALESCE(granted_by, ''), created_at, updated_at`
	var rec CollaboratorRecord
	if err := p.db.QueryRowContext(ctx, q, name, userID, role, grantedBy).Scan(
		&rec.ServerName, &rec.UserID, &rec.Role, &rec.GrantedBy, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	if err := p.db.QueryRowContext(ctx,
		`SELECT username, COALESCE(email, ''), role::text FROM users WHERE id = $1`, userID).
		Scan(&rec.Username, &rec.Email, &rec.UserRole); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (p *PGRepo) DeleteServerCollaborator(ctx context.Context, name, userID string) error {
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM server_collaborators WHERE server_name = $1 AND user_id = $2`,
		name, userID)
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
