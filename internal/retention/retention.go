// Package retention deletes the control-plane rows that stopped mattering, so
// the sign-in tables and the audit trail do not grow for as long as an install
// runs.
//
// Every sign-in door leaves a row behind: a session per login, an email code or
// passkey challenge per attempt, a bind code per /felis link, an op-login request
// per staff sign-in, a setup token per invite. Each is spent once it expires,
// is used, or is revoked, and a spent row only holds a hash nothing accepts any
// more. Prune drops those rows Grace after they were spent: long enough that an
// incident can still line up a session's device and address with the audit
// trail, short enough that the tables stay the size of what is live.
//
// Audit rows are kept for the install's [audit] retention and then deleted, oldest
// first. `felis db audit-export` (ExportAudit) writes the rows to a file first
// for an install that must keep them longer.
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

// Grace is how long a spent sign-in row outlives the moment it was spent.
const Grace = 30 * 24 * time.Hour

// batch bounds one DELETE, so a first run on an install that never pruned stays
// clear of the pool's statement timeout and holds its row locks briefly.
const batch = 5000

// sweep is one table's spent rows: every statement takes the cutoff as $1 and a
// row limit as $2, and deletes at most $2 rows spent before the cutoff.
type sweep struct {
	table string
	where string
}

// sweeps name each sign-in table's spent rows. A session is spent once it
// expired or was revoked; a single-use code or challenge once it expired or was
// redeemed; a bind code once it expired (redeeming deletes it); a migration that
// never completed once nobody touched it for Grace (a redeemed one is the record
// of which account moved where and is kept); an OTP failure window once it began
// Grace ago (a window lasts a day).
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

// Policy is what Prune keeps.
type Policy struct {
	// Audit is how long audit rows are kept; 0 keeps every row.
	Audit time.Duration
}

// Result counts the rows Prune deleted, by table.
type Result map[string]int64

// Total is the number of rows deleted across all tables.
func (r Result) Total() int64 {
	var n int64
	for _, v := range r {
		n += v
	}
	return n
}

// Prune deletes the spent sign-in rows and the audit rows past the policy's
// retention. It stops at the first failing table and returns what it deleted
// so far with the error; the next run picks up from there.
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

// Loop runs Prune a minute after it starts and then every interval until ctx
// ends, logging what each run deleted.
func Loop(ctx context.Context, db *sql.DB, p Policy, every time.Duration, log *slog.Logger) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		res, err := Prune(ctx, db, time.Now(), p)
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

// AuditRow is one audit_logs row as ExportAudit writes it.
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

// ExportAudit writes the audit rows created in [since, until) to w as JSON lines,
// oldest first; a zero bound is open. It returns how many rows it wrote.
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
