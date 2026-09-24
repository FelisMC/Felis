package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"felis.lolicon.best/internal/dbbackup"
)

// The panel's backup card reads one flag, stale, so these pin when it is set:
// never backed up, too old, and not for a fresh backup. A store outage must
// not read as either answer.

func getDBBackup(t *testing.T, api *API) (int, dbBackupView) {
	t.Helper()
	w := do(api.ExternalHandler(), "GET", "/api/v1/platform/db-backup", "", nil)
	var v dbBackupView
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
	}
	return w.Code, v
}

func TestDBBackupNeverRecordedIsStale(t *testing.T) {
	api, _ := seedUpdatesAPI(t)
	code, v := getDBBackup(t, api)
	if code != http.StatusOK || v.Last != nil || !v.Stale {
		t.Fatalf("never backed up = %d %+v, want 200 last=null stale", code, v)
	}
	if v.MaxAgeSeconds != int64(dbbackup.StaleAfter/time.Second) {
		t.Fatalf("max_age_seconds = %d", v.MaxAgeSeconds)
	}
}

func TestDBBackupFreshness(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		age   time.Duration
		stale bool
	}{
		{"taken this morning", 8 * time.Hour, false},
		{"yesterday's, timer slightly late", 25 * time.Hour, false},
		{"missed a day", 27 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, repo := seedUpdatesAPI(t)
			api.Now = func() time.Time { return now }
			st := dbbackup.Status{At: now.Add(-tc.age), Name: "felis-db-x-daily.tar", Label: "daily", SizeBytes: 4096, SchemaVersion: 31, Dir: "/var/lib/felis/db-backups"}
			raw, _ := json.Marshal(st)
			repo.settings[dbbackup.StatusKey] = raw

			code, v := getDBBackup(t, api)
			if code != http.StatusOK || v.Last == nil {
				t.Fatalf("code %d, view %+v", code, v)
			}
			if v.Stale != tc.stale {
				t.Fatalf("stale = %v, want %v", v.Stale, tc.stale)
			}
			if v.Last.Name != st.Name || v.Last.SizeBytes != 4096 || v.Last.SchemaVersion != 31 || !v.Last.At.Equal(st.At) {
				t.Fatalf("record not passed through: %+v", v.Last)
			}
		})
	}
}

func TestDBBackupStoreOutageIsAnError(t *testing.T) {
	api, repo := seedUpdatesAPI(t)
	repo.failGetSetting = errors.New("connection reset")
	if code, _ := getDBBackup(t, api); code == http.StatusOK {
		t.Fatal("a failed settings read answered 200")
	}
}

func TestDBBackupAdminOnly(t *testing.T) {
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "u1", Role: "user"}}
	if code, _ := getDBBackup(t, api); code != http.StatusForbidden {
		t.Fatalf("player read = %d, want 403", code)
	}
}
