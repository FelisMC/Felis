package watchdog

import (
	"context"
	"database/sql"
)

// OwnerEmails returns the verified addresses of the enabled owner accounts, the
// people who can act on an alert, in order. internal/pgint holds its contract
// against the real schema.
func OwnerEmails(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT email FROM users
		 WHERE role = 'owner' AND email_verified AND COALESCE(email, '') <> ''
		   AND NOT disabled AND deleted_at IS NULL
		 ORDER BY email`)
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
