//go:build pgint

package pgint

import (
	"bytes"
	"context"
	"regexp"
	"testing"
	"time"

	"felis.lolicon.best/internal/retention"
)

func mustExec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func rowExists(t *testing.T, q string, args ...any) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRow(`SELECT EXISTS (`+q+`)`, args...).Scan(&ok); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return ok
}

// Prune deletes each sign-in row a month after it was spent (expired, used or
// revoked) and keeps the ones spent more recently and the live ones; audit rows
// go once they are older than the policy's retention.
func TestPruneDropsRowsSpentLongerThanGrace(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	old := now.Add(-31 * 24 * time.Hour)    // spent past the 30-day grace
	recent := now.Add(-29 * 24 * time.Hour) // spent inside it
	future := now.Add(time.Hour)
	u := newUser(t, "user", "prune")
	src1, src2, src3 := newUser(t, "user", "mig1"), newUser(t, "user", "mig2"), newUser(t, "user", "mig3")
	tag := suffix(t)
	id := func(s string) string { return s + "-" + tag }

	for _, r := range []struct {
		name               string
		expires, used, rev any
	}{
		{"s-expired-old", old, nil, nil},
		{"s-revoked-old", future, nil, old},
		{"s-expired-recent", recent, nil, nil},
		{"s-revoked-recent", future, nil, recent},
		{"s-live", future, nil, nil},
	} {
		mustExec(t, `INSERT INTO sessions (token_hash, user_id, expires_at, revoked_at) VALUES ($1, $2, $3, $4)`,
			id(r.name), u.ID, r.expires, r.rev)
	}
	for _, r := range []struct {
		name          string
		expires, used any
	}{
		{"expired-old", old, nil},
		{"used-old", future, old},
		{"expired-recent", recent, nil},
		{"used-recent", future, recent},
		{"live", future, nil},
	} {
		mustExec(t, `INSERT INTO email_otps (id, user_id, email, code_hash, purpose, expires_at, consumed_at)
			VALUES ($1, $2, 'p@example.net', 'h', 'login_email', $3, $4)`, id("otp-"+r.name), u.ID, r.expires, r.used)
		mustExec(t, `INSERT INTO webauthn_challenges (id, user_id, purpose, session_data, expires_at, consumed_at)
			VALUES ($1, $2, 'passkey_register', '\x00', $3, $4)`, id("wc-"+r.name), u.ID, r.expires, r.used)
		mustExec(t, `INSERT INTO webauthn_discoverable_challenges (id, session_data, expires_at, consumed_at)
			VALUES ($1, '\x00', $2, $3)`, id("wd-"+r.name), r.expires, r.used)
		mustExec(t, `INSERT INTO setup_tokens (token_hash, user_id, expires_at, consumed_at) VALUES ($1, $2, $3, $4)`,
			id("st-"+r.name), u.ID, r.expires, r.used)
		mustExec(t, `INSERT INTO op_login_requests (id, user_id, email, expires_at, consumed_at)
			VALUES ($1, $2, 'staff@example.net', $3, $4)`, id("op-"+r.name), u.ID, r.expires, r.used)
	}
	for _, r := range []struct {
		name    string
		expires time.Time
	}{{"lc-old", old}, {"lc-recent", recent}, {"lc-live", future}} {
		mustExec(t, `INSERT INTO account_link_codes (code, mc_uuid, expires_at) VALUES ($1, $2, $3)`,
			id(r.name), testUUID(t), r.expires)
	}
	// A migration abandoned a month ago goes; one touched recently stays, and a
	// redeemed one is the record of the move and stays however old it is.
	mustExec(t, `INSERT INTO account_migrations (id, source_user_id, state, created_at, updated_at) VALUES ($1, $2, 'initiated', $3, $3)`,
		id("mig-stale"), src1.ID, old)
	mustExec(t, `INSERT INTO account_migrations (id, source_user_id, state, created_at, updated_at) VALUES ($1, $2, 'confirmed', $3, $4)`,
		id("mig-fresh"), src2.ID, old, recent)
	mustExec(t, `INSERT INTO account_migrations (id, source_user_id, state, redeemed_at, created_at, updated_at) VALUES ($1, $2, 'redeemed', $3, $3, $3)`,
		id("mig-redeemed"), src3.ID, old)
	mustExec(t, `INSERT INTO otp_failure_windows (user_id, purpose, window_start, failures) VALUES ($1, 'login_email', $2, 3)`, u.ID, old)
	mustExec(t, `INSERT INTO otp_failure_windows (user_id, purpose, window_start, failures) VALUES ($1, 'onboard_email', $2, 3)`, u.ID, recent)
	mustExec(t, `INSERT INTO audit_logs (actor, source, action, request_id, created_at) VALUES ('pgint', 'external', 'prune.test', $1, $2)`,
		id("audit-61d"), now.Add(-61*24*time.Hour))
	mustExec(t, `INSERT INTO audit_logs (actor, source, action, request_id, created_at) VALUES ('pgint', 'external', 'prune.test', $1, $2)`,
		id("audit-59d"), now.Add(-59*24*time.Hour))

	if _, err := retention.Prune(ctx, db, now, retention.Policy{Audit: 60 * 24 * time.Hour}); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for _, c := range []struct {
		q, key string
		want   bool
	}{
		{`SELECT 1 FROM sessions WHERE token_hash = $1`, "s-expired-old", false},
		{`SELECT 1 FROM sessions WHERE token_hash = $1`, "s-revoked-old", false},
		{`SELECT 1 FROM sessions WHERE token_hash = $1`, "s-expired-recent", true},
		{`SELECT 1 FROM sessions WHERE token_hash = $1`, "s-revoked-recent", true},
		{`SELECT 1 FROM sessions WHERE token_hash = $1`, "s-live", true},
		{`SELECT 1 FROM account_link_codes WHERE code = $1`, "lc-old", false},
		{`SELECT 1 FROM account_link_codes WHERE code = $1`, "lc-recent", true},
		{`SELECT 1 FROM account_link_codes WHERE code = $1`, "lc-live", true},
		{`SELECT 1 FROM account_migrations WHERE id = $1`, "mig-stale", false},
		{`SELECT 1 FROM account_migrations WHERE id = $1`, "mig-fresh", true},
		{`SELECT 1 FROM account_migrations WHERE id = $1`, "mig-redeemed", true},
		{`SELECT 1 FROM audit_logs WHERE request_id = $1`, "audit-61d", false},
		{`SELECT 1 FROM audit_logs WHERE request_id = $1`, "audit-59d", true},
	} {
		if got := rowExists(t, c.q, id(c.key)); got != c.want {
			t.Errorf("%s: present = %v, want %v", c.key, got, c.want)
		}
	}
	for _, table := range []struct{ name, q, prefix string }{
		{"email_otps", `SELECT 1 FROM email_otps WHERE id = $1`, "otp-"},
		{"webauthn_challenges", `SELECT 1 FROM webauthn_challenges WHERE id = $1`, "wc-"},
		{"webauthn_discoverable_challenges", `SELECT 1 FROM webauthn_discoverable_challenges WHERE id = $1`, "wd-"},
		{"setup_tokens", `SELECT 1 FROM setup_tokens WHERE token_hash = $1`, "st-"},
		{"op_login_requests", `SELECT 1 FROM op_login_requests WHERE id = $1`, "op-"},
	} {
		for _, c := range []struct {
			name string
			want bool
		}{{"expired-old", false}, {"used-old", false}, {"expired-recent", true}, {"used-recent", true}, {"live", true}} {
			if got := rowExists(t, table.q, id(table.prefix+c.name)); got != c.want {
				t.Errorf("%s %s: present = %v, want %v", table.name, c.name, got, c.want)
			}
		}
	}
	if rowExists(t, `SELECT 1 FROM otp_failure_windows WHERE user_id = $1 AND purpose = 'login_email'`, u.ID) {
		t.Error("an OTP failure window that began a month ago survived")
	}
	if !rowExists(t, `SELECT 1 FROM otp_failure_windows WHERE user_id = $1 AND purpose = 'onboard_email'`, u.ID) {
		t.Error("an OTP failure window inside the grace was deleted")
	}
}

// With no audit retention every audit row stays, and a table with more spent rows
// than one DELETE takes is emptied of them in the same run.
func TestPruneKeepsAuditForeverAndDrainsLargeBacklogs(t *testing.T) {
	ctx := context.Background()
	now := mustNow()
	tag := suffix(t)
	mustExec(t, `INSERT INTO audit_logs (actor, source, action, request_id, created_at) VALUES ('pgint', 'external', 'prune.test', $1, $2)`,
		"ancient-"+tag, now.Add(-3000*24*time.Hour))
	mustExec(t, `INSERT INTO account_link_codes (code, mc_uuid, expires_at)
		SELECT $1 || n, gen_random_uuid(), $2 FROM generate_series(1, 5003) AS n`, "bulk-"+tag+"-", now.Add(-40*24*time.Hour))

	res, err := retention.Prune(ctx, db, now, retention.Policy{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !rowExists(t, `SELECT 1 FROM audit_logs WHERE request_id = $1`, "ancient-"+tag) {
		t.Error("an audit row was deleted with retention off")
	}
	if _, ok := res["audit_logs"]; ok {
		t.Errorf("audit_logs was pruned with retention off: %v", res)
	}
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM account_link_codes WHERE code LIKE $1`, "bulk-"+tag+"-%").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d of 5003 spent bind codes left after one run", left)
	}
	if res["account_link_codes"] < 5003 {
		t.Errorf("account_link_codes deleted = %d, want at least 5003", res["account_link_codes"])
	}
}

var idRE = regexp.MustCompile(`"id":\d+`)

// ExportAudit writes the rows inside [since, until) as JSON lines, oldest first,
// with the optional columns left out when empty.
func TestExportAuditWritesTheWindow(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "export")
	tag := suffix(t)
	base := time.Date(2031, 3, 4, 0, 0, 0, 0, time.UTC)
	mustExec(t, `INSERT INTO audit_logs (actor, source, action, request_id, created_at) VALUES ('before', 'external', 'x.before', $1, $2)`,
		"exp-"+tag, base.Add(-time.Second))
	mustExec(t, `INSERT INTO audit_logs (actor, actor_user_id, source, action, server_name, request_id, client_ip, user_agent, payload, created_at)
		VALUES ('alice@example.net', $1, 'external', 'server.patch', 'survival', $2, '192.0.2.7', 'curl/8', '{"display_name":"Survival"}', $3)`,
		u.ID, "exp-"+tag, base.Add(2*time.Hour))
	mustExec(t, `INSERT INTO audit_logs (actor, source, action, created_at) VALUES ('internal', 'internal:velocity', 'server.wake', $1)`,
		base.Add(time.Hour))
	mustExec(t, `INSERT INTO audit_logs (actor, source, action, request_id, created_at) VALUES ('after', 'external', 'x.after', $1, $2)`,
		"exp-"+tag, base.Add(24*time.Hour))

	var buf bytes.Buffer
	n, err := retention.ExportAudit(ctx, db, base, base.Add(24*time.Hour), &buf)
	if err != nil {
		t.Fatalf("ExportAudit: %v", err)
	}
	// Row ids are serial; blank them to compare the rest literally.
	got := idRE.ReplaceAllString(buf.String(), `"id":0`)
	want := `{"id":0,"created_at":"2031-03-04T01:00:00Z","actor":"internal","source":"internal:velocity","action":"server.wake"}` + "\n" +
		`{"id":0,"created_at":"2031-03-04T02:00:00Z","actor":"alice@example.net","actor_user_id":"` + u.ID + `","source":"external","action":"server.patch","server_name":"survival","request_id":"exp-` + tag + `","client_ip":"192.0.2.7","user_agent":"curl/8","payload":{"display_name":"Survival"}}` + "\n"
	if n != 2 || got != want {
		t.Fatalf("ExportAudit wrote %d rows:\n%s\nwant 2:\n%s", n, got, want)
	}
}
