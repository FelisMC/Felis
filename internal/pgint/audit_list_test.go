//go:build pgint

package pgint

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

func TestListAuditFiltersAndPages(t *testing.T) {
	ctx := context.Background()
	tag := suffix(t)
	u := newUser(t, "user", "audit")
	base := time.Date(2033, 5, 6, 0, 0, 0, 0, time.UTC)
	patch, start, create := "pgaudit-"+tag+".server.patch", "pgaudit-"+tag+".server.start", "pgaudit-"+tag+".user.create"
	srvA, srvB := "aud-a-"+tag, "aud-b-"+tag

	add := func(actor, action string, server, payload any, at time.Time) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRow(`INSERT INTO audit_logs (actor, source, action, server_name, payload, created_at)
			VALUES ($1, 'external', $2, $3, $4::jsonb, $5) RETURNING id`, actor, action, server, payload, at).Scan(&id); err != nil {
			t.Fatalf("insert audit row: %v", err)
		}
		return id
	}
	r1 := add("Alice-"+tag+"@example.net", patch, srvA, `{"image_from":"a","image_to":"b"}`, base.Add(time.Hour))
	r2 := add("bob-"+tag, start, srvB, `{"note":"100% done"}`, base.Add(2*time.Hour))
	r3 := add("bob-"+tag, create, nil, nil, base.Add(3*time.Hour))
	r4 := add("carol-"+tag, patch, srvA, `{"note":"1000 done"}`, base.Add(4*time.Hour))
	mustExec(t, `UPDATE audit_logs SET actor_user_id = $1, client_ip = '192.0.2.7', user_agent = 'curl/8', request_id = $2 WHERE id = $3`,
		u.ID, "req-"+tag, r1)

	ids := func(rows []api.AuditRecord) []int64 {
		out := make([]int64, len(rows))
		for i, r := range rows {
			out[i] = r.ID
		}
		return out
	}
	list := func(q api.AuditQuery) []int64 {
		t.Helper()
		if q.Limit == 0 {
			q.Limit = 50
		}
		rows, err := repo.ListAudit(ctx, q)
		if err != nil {
			t.Fatalf("ListAudit(%+v): %v", q, err)
		}
		return ids(rows)
	}

	for _, tc := range []struct {
		name string
		q    api.AuditQuery
		want []int64
	}{
		{"actor substring ignores case", api.AuditQuery{Actor: "ALICE-" + tag}, []int64{r1}},
		{"action prefix", api.AuditQuery{Action: "pgaudit-" + tag + ".server."}, []int64{r4, r2, r1}},
		{"action prefix reads _ literally", api.AuditQuery{Action: "pgaudit-" + tag + ".server_"}, nil},
		{"server is exact", api.AuditQuery{Server: srvA}, []int64{r4, r1}},
		{"server is not a prefix", api.AuditQuery{Server: srvA[:len(srvA)-1], Actor: tag}, nil},
		{"since is inclusive", api.AuditQuery{Actor: tag, Since: base.Add(2 * time.Hour)}, []int64{r4, r3, r2}},
		{"until is exclusive", api.AuditQuery{Actor: tag, Until: base.Add(3 * time.Hour)}, []int64{r2, r1}},
		{"since and until together", api.AuditQuery{Actor: tag, Since: base.Add(2 * time.Hour), Until: base.Add(4 * time.Hour)}, []int64{r3, r2}},
		{"keyword in actor", api.AuditQuery{Keyword: "BOB-" + tag}, []int64{r3, r2}},
		{"keyword in action", api.AuditQuery{Keyword: ".user.create", Actor: tag}, []int64{r3}},
		{"keyword in server name", api.AuditQuery{Keyword: srvB}, []int64{r2}},
		{"keyword in payload", api.AuditQuery{Keyword: "IMAGE_TO", Actor: tag}, []int64{r1}},
		{"keyword reads % literally", api.AuditQuery{Keyword: "100%", Actor: tag}, []int64{r2}},
		{"keyword reads _ literally", api.AuditQuery{Keyword: "0_0", Actor: tag}, nil},
		{"keyword accepts a backslash", api.AuditQuery{Keyword: `\`, Actor: tag}, nil},
		{"filters combine", api.AuditQuery{Actor: "bob-" + tag, Action: "pgaudit-" + tag + ".server.", Keyword: "done"}, []int64{r2}},
	} {
		if got := list(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	var paged []int64
	var before int64
	for {
		page := list(api.AuditQuery{Actor: tag, Limit: 3, BeforeID: before})
		if len(page) == 0 {
			break
		}
		if len(page) > 3 {
			t.Fatalf("page of %d rows exceeds the limit", len(page))
		}
		paged = append(paged, page...)
		before = page[len(page)-1]
	}
	if want := []int64{r4, r3, r2, r1}; !slices.Equal(paged, want) {
		t.Errorf("pages joined = %v, want %v", paged, want)
	}

	rows, err := repo.ListAudit(ctx, api.AuditQuery{Actor: tag, Limit: 4})
	if err != nil || len(rows) != 4 {
		t.Fatalf("ListAudit = %v, %v", rows, err)
	}
	bare, full := rows[1], rows[3]
	if bare.ID != r3 || bare.ActorUserID != nil || bare.ServerName != nil || bare.RequestID != nil ||
		bare.ClientIP != nil || bare.UserAgent != nil || bare.Payload != nil {
		t.Errorf("row without optional columns = %+v, want them all nil", bare)
	}
	var payload map[string]string
	if err := json.Unmarshal(full.Payload, &payload); err != nil ||
		!reflect.DeepEqual(payload, map[string]string{"image_from": "a", "image_to": "b"}) {
		t.Errorf("payload = %s, %v", full.Payload, err)
	}
	if full.ID != r1 || full.Actor != "Alice-"+tag+"@example.net" || full.Source != "external" || full.Action != patch ||
		full.ActorUserID == nil || *full.ActorUserID != u.ID || full.ServerName == nil || *full.ServerName != srvA ||
		full.RequestID == nil || *full.RequestID != "req-"+tag || full.ClientIP == nil || *full.ClientIP != "192.0.2.7" ||
		full.UserAgent == nil || *full.UserAgent != "curl/8" || !full.CreatedAt.Equal(base.Add(time.Hour)) {
		t.Errorf("fully populated row = %+v", full)
	}
}

func TestCountAuthFailuresCountsRecentFailedSignIns(t *testing.T) {
	ctx := context.Background()
	tag := suffix(t)
	now := mustNow()
	since := now.Add(-24 * time.Hour)
	count := func(from time.Time) int {
		t.Helper()
		n, err := repo.CountAuthFailures(ctx, from)
		if err != nil {
			t.Fatalf("CountAuthFailures: %v", err)
		}
		return n
	}
	day, wide := count(since), count(since.Add(-2*time.Hour))

	for action, at := range map[string]time.Time{
		"auth." + tag + ".failed":       now.Add(-time.Hour),
		"auth." + tag + ".failedish":    now.Add(-time.Hour),
		"auth." + tag + ".otp.locked":   now.Add(-time.Hour),
		"auth.other-" + tag + ".failed": now.Add(-25 * time.Hour),
	} {
		mustExec(t, `INSERT INTO audit_logs (actor, source, action, created_at) VALUES ('pgint', 'external', $1, $2)`, action, at)
	}

	if got := count(since) - day; got != 1 {
		t.Errorf("failures in the last 24 hours rose by %d, want 1 (only the recent auth.*.failed row)", got)
	}
	if got := count(since.Add(-2*time.Hour)) - wide; got != 2 {
		t.Errorf("failures since 26 hours ago rose by %d, want 2 (both auth.*.failed rows)", got)
	}
}
