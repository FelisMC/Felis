// Package retention prunes spent sign-in records and audit rows past their retention period.
package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// Grace retains spent sign-in records for incident correlation.
const Grace = 30 * 24 * time.Hour

// Bound each DELETE to keep statement duration and row-lock lifetime short.
const batch = 5000

// Each sweep binds the cutoff as $1 and row limit as $2.
type sweep struct {
	table string
	where string
}

// Keep redeemed account migrations; prune only abandoned migrations after Grace.
var sweeps = []sweep{
	{"sessions", "expires_at < $1 OR revoked_at < $1"},
	{"email_otps", "expires_at < $1 OR consumed_at < $1"},
	{"webauthn_challenges", "expires_at < $1 OR consumed_at < $1"},
	{"webauthn_discoverable_challenges", "expires_at < $1 OR consumed_at < $1"},
	{"setup_tokens", "expires_at < $1 OR consumed_at < $1"},
	{"op_login_requests", "expires_at < $1 OR consumed_at < $1"},
	{"account_link_codes", "expires_at < $1"},
	{"account_migrations", "state <> 'redeemed' AND updated_at < $1"},
	{"otp_failure_windows", "window_start < $1"},
}

type Policy struct {
	// Audit=0 retains all audit rows.
	Audit time.Duration
}

type Result map[string]int64

func (r Result) Total() int64 {
	var n int64
	for _, v := range r {
		n += v
	}
	return n
}

// Prune stops at the first failing table and returns the deletion counts accumulated so far.
func Prune(ctx context.Context, db *sql.DB, now time.Time, p Policy) (Result, error) {
	res := Result{}
	for _, s := range sweeps {
		n, err := deleteBatched(ctx, db, s.table, s.where, now.Add(-Grace))
		res[s.table] = n
		if err != nil {
			return res, fmt.Errorf("prune %s: %w", s.table, err)
		}
	}
	if p.Audit > 0 {
		n, err := deleteBatched(ctx, db, "audit_logs", "created_at < $1", now.Add(-p.Audit))
		res["audit_logs"] = n
		if err != nil {
			return res, fmt.Errorf("prune audit_logs: %w", err)
		}
	}
	return res, nil
}

func deleteBatched(ctx context.Context, db *sql.DB, table, where string, cutoff time.Time) (int64, error) {
	q := fmt.Sprintf(`DELETE FROM %[1]s WHERE ctid IN (SELECT ctid FROM %[1]s WHERE %[2]s LIMIT $2)`, table, where)
	var total int64
	for {
		r, err := db.ExecContext(ctx, q, cutoff, batch)
		if err != nil {
			return total, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < batch {
			return total, nil
		}
	}
}

// Loop reads policy on every run so retention changes need no restart.
func Loop(ctx context.Context, db *sql.DB, policy func(context.Context) Policy, every time.Duration, log *slog.Logger) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		res, err := Prune(ctx, db, time.Now(), policy(ctx))
		if err != nil {
			log.Error("retention: prune failed", "err", err, "deleted", res.Total())
		} else if res.Total() > 0 {
			args := []any{"deleted", res.Total()}
			for _, s := range sweeps {
				if res[s.table] > 0 {
					args = append(args, s.table, res[s.table])
				}
			}
			if res["audit_logs"] > 0 {
				args = append(args, "audit_logs", res["audit_logs"])
			}
			log.Info("retention: pruned spent rows", args...)
		}
		t.Reset(every)
	}
}

type AuditRow struct {
	ID          int64           `json:"id"`
	CreatedAt   time.Time       `json:"created_at"`
	Actor       string          `json:"actor"`
	ActorUserID string          `json:"actor_user_id,omitempty"`
	Source      string          `json:"source"`
	Action      string          `json:"action"`
	ServerName  string          `json:"server_name,omitempty"`
	RequestID   string          `json:"request_id,omitempty"`
	ClientIP    string          `json:"client_ip,omitempty"`
	UserAgent   string          `json:"user_agent,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// ExportAudit writes JSON lines in [since, until), oldest first; zero bounds are open.
func ExportAudit(ctx context.Context, db *sql.DB, since, until time.Time, w io.Writer) (int, error) {
	const q = `SELECT id, created_at, actor, COALESCE(actor_user_id, ''), source, action,
		COALESCE(server_name, ''), COALESCE(request_id, ''), COALESCE(host(client_ip), ''),
		COALESCE(user_agent, ''), COALESCE(payload::text, '')
		FROM audit_logs
		WHERE ($1::timestamptz IS NULL OR created_at >= $1) AND ($2::timestamptz IS NULL OR created_at < $2)
		ORDER BY created_at, id`
	rows, err := db.QueryContext(ctx, q, nullTime(since), nullTime(until))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	n := 0
	for rows.Next() {
		var (
			r       AuditRow
			payload string
		)
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.Actor, &r.ActorUserID, &r.Source, &r.Action,
			&r.ServerName, &r.RequestID, &r.ClientIP, &r.UserAgent, &payload); err != nil {
			return n, err
		}
		r.CreatedAt = r.CreatedAt.UTC()
		if payload != "" {
			r.Payload = json.RawMessage(payload)
		}
		if err := enc.Encode(r); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func nullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}
