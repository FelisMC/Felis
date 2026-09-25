package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"felis.lolicon.best/internal/updates"
)

// The Updates page's version card reads the report and one flag, stale: set when
// no check was ever recorded and when the daily timer stopped, clear for today's.

func TestUpdateReportNeverRecorded(t *testing.T) {
	api, _ := seedUpdatesAPI(t)
	w := do(api.ExternalHandler(), "GET", "/api/v1/updates/report", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != `{"report":null,"stale":true,"max_age_seconds":93600}`+"\n" {
		t.Fatalf("never checked = %d %s", w.Code, w.Body.String())
	}
}

func TestUpdateReportPassesTheRecordThrough(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	api, repo := seedUpdatesAPI(t)
	api.Now = func() time.Time { return now }
	repo.settings[updates.StatusKey] = []byte(`{"checked_at":"2026-09-25T03:00:00Z","felis":"v0.4.0","components":[` +
		`{"name":"felis-api","current":"v0.4.0","latest":"v0.5.0","state":"available","selector":"panel"},` +
		`{"name":"k3s","current":"v1.36.2+k3s1","state":"unknown","selector":"k3s","error":"github: HTTP 403"}]}`)
	w := do(api.ExternalHandler(), "GET", "/api/v1/updates/report", "", nil)
	want := `{"report":{"checked_at":"2026-09-25T03:00:00Z","felis":"v0.4.0","components":[` +
		`{"name":"felis-api","current":"v0.4.0","latest":"v0.5.0","state":"available","selector":"panel"},` +
		`{"name":"k3s","current":"v1.36.2+k3s1","state":"unknown","selector":"k3s","error":"github: HTTP 403"}]},` +
		`"stale":false,"max_age_seconds":93600}` + "\n"
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("report = %d\n%s\nwant\n%s", w.Code, w.Body.String(), want)
	}
}

func TestUpdateReportFreshness(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		checkedAt string
		stale     bool
	}{
		{"checked last night", "2026-09-25T03:00:00Z", false},
		{"yesterday's, timer slightly late", "2026-09-24T11:00:00Z", false},
		{"timer missed a day", "2026-09-24T09:00:00Z", true},
		{"no timestamp", "0001-01-01T00:00:00Z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, repo := seedUpdatesAPI(t)
			api.Now = func() time.Time { return now }
			repo.settings[updates.StatusKey] = []byte(`{"checked_at":"` + tc.checkedAt + `","felis":"v0.4.0","components":null}`)
			w := do(api.ExternalHandler(), "GET", "/api/v1/updates/report", "", nil)
			want := `{"report":{"checked_at":"` + tc.checkedAt + `","felis":"v0.4.0","components":[]},"stale":false,"max_age_seconds":93600}` + "\n"
			if tc.stale {
				want = `{"report":{"checked_at":"` + tc.checkedAt + `","felis":"v0.4.0","components":[]},"stale":true,"max_age_seconds":93600}` + "\n"
			}
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Fatalf("report = %d %s, want %s", w.Code, w.Body.String(), want)
			}
		})
	}
}

func TestUpdateReportStoreOutageIsAnError(t *testing.T) {
	api, repo := seedUpdatesAPI(t)
	repo.failGetSetting = errors.New("connection reset")
	if w := do(api.ExternalHandler(), "GET", "/api/v1/updates/report", "", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("a failed settings read answered %d", w.Code)
	}
}

func TestUpdateReportAdminOnly(t *testing.T) {
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "u1", Role: "user"}}
	if w := do(api.ExternalHandler(), "GET", "/api/v1/updates/report", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("player read = %d, want 403", w.Code)
	}
}
