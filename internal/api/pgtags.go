package api

import (
	"context"
	"encoding/json"
	"fmt"
)

// decodeServerTags reads a to_jsonb(tags) column; an empty set is nil.
func decodeServerTags(raw []byte) ([]string, error) {
	var tags []string
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil, fmt.Errorf("server tags: %w", err)
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

func (p *PGRepo) ServerTags(ctx context.Context) (map[string][]string, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT name, to_jsonb(tags) FROM servers WHERE deleted_at IS NULL AND cardinality(tags) > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var name string
		var raw []byte
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		if out[name], err = decodeServerTags(raw); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}

func (p *PGRepo) SetServerTags(ctx context.Context, name string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	res, err := p.db.ExecContext(ctx,
		`UPDATE servers SET tags = $2 WHERE name = $1 AND deleted_at IS NULL`, name, tags)
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
