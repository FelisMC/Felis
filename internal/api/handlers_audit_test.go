package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"
)

const auditPath = "/api/v1/platform/audit"

func seedAuditRows(ids ...int64) []AuditRecord {
	rows := make([]AuditRecord, len(ids))
	for i, id := range ids {
		rows[i] = AuditRecord{ID: id, CreatedAt: time.Date(2026, 10, 1, 0, 0, int(id), 0, time.UTC),
			Actor: "owner@example.net", Source: "external", Action: "server.patch"}
	}
	return rows
}

func TestListAuditPagesNewestFirst(t *testing.T) {
	a, repo, _ := ownerAPI()
	repo.auditRows = seedAuditRows(1, 2, 3, 4, 5)
	for _, tc := range []struct {
		query  string
		ids    []int64
		next   string
		before int64
	}{
		{"?limit=2", []int64{5, 4}, "4", 0},
		{"?limit=2&cursor=4", []int64{3, 2}, "2", 4},
		{"?limit=2&cursor=2", []int64{1}, "", 2},
		{"?limit=5", []int64{5, 4, 3, 2, 1}, "", 0},
		{"", []int64{5, 4, 3, 2, 1}, "", 0},
	} {
		w := do(a.ExternalHandler(), "GET", auditPath+tc.query, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", tc.query, w.Code, w.Body.String())
		}
		var page struct {
			Entries []struct {
				ID int64 `json:"id"`
			} `json:"entries"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		ids := make([]int64, len(page.Entries))
		for i, e := range page.Entries {
			ids[i] = e.ID
		}
		if !slices.Equal(ids, tc.ids) || page.NextCursor != tc.next || repo.auditQuery.BeforeID != tc.before {
			t.Errorf("GET %s: ids %v next %q before %d, want %v %q %d",
				tc.query, ids, page.NextCursor, repo.auditQuery.BeforeID, tc.ids, tc.next, tc.before)
		}
	}
}

func TestListAuditPassesFiltersToTheStore(t *testing.T) {
	a, repo, _ := ownerAPI()
	query := "?actor=alice&action=server.&server=survival&q=100%25&since=2026-10-01T00:00:00Z&until=2026-10-02T08:00:00%2B08:00"
	if w := do(a.ExternalHandler(), "GET", auditPath+query, "", nil); w.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	got := repo.auditQuery
	if got.Actor != "alice" || got.Action != "server." || got.Server != "survival" || got.Keyword != "100%" ||
		!got.Since.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) ||
		!got.Until.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("store query = %+v", got)
	}
}

func TestListAuditRejectsBadParameters(t *testing.T) {
	a, repo, _ := ownerAPI()
	for _, query := range []string{
		"?limit=0", "?limit=201", "?limit=many",
		"?cursor=0", "?cursor=latest",
		"?since=yesterday", "?until=2026-13-01T00:00:00Z",
		"?since=2026-10-02T00:00:00Z&until=2026-10-01T00:00:00Z",
		"?since=2026-10-01T00:00:00Z&until=2026-10-01T00:00:00Z",
	} {
		w := do(a.ExternalHandler(), "GET", auditPath+query, "", nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Errorf("GET %s = %d %s", query, w.Code, w.Body.String())
		}
	}
	if repo.auditQuery.Limit != 0 {
		t.Fatal("a refused query reached the store")
	}
}

func TestListAuditKeepsPayloadAndNulls(t *testing.T) {
	a, repo, _ := ownerAPI()
	server := "survival"
	repo.auditRows = []AuditRecord{
		{ID: 2, CreatedAt: time.Date(2026, 10, 1, 0, 0, 2, 0, time.UTC), Actor: "owner@example.net", Source: "external",
			Action: "server.patch", ServerName: &server, Payload: json.RawMessage(`{"image_from":"a","image_to":"b"}`)},
		{ID: 1, CreatedAt: time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC), Actor: "velocity", Source: "internal:velocity",
			Action: "server.wake"},
	}
	w := do(a.ExternalHandler(), "GET", auditPath, "", nil)
	var page struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Entries) != 2 {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	payload, _ := page.Entries[0]["payload"].(map[string]any)
	if payload["image_from"] != "a" || payload["image_to"] != "b" || page.Entries[0]["server_name"] != "survival" {
		t.Errorf("first entry = %v", page.Entries[0])
	}
	for _, key := range []string{"actor_user_id", "server_name", "request_id", "client_ip", "user_agent", "payload"} {
		if v, ok := page.Entries[1][key]; !ok || v != nil {
			t.Errorf("second entry %s = %v (present %v), want null", key, v, ok)
		}
	}
}
