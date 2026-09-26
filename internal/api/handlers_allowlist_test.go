package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAllowlistRoutes covers GET /servers/{name}/allowlist and PUT
// /servers/{name}/allowlist/{uuid}: the owner and an admin read and change the
// list, anyone else is refused before the repo is touched, a UUID that is not on
// the list is 404, and each change is audited with the UUID it touched.
func TestAllowlistRoutes(t *testing.T) {
	const friend = "0f8fad5b-d9cb-469f-a165-70867728950e"
	const other = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	admin := &Principal{UserID: "admin1", Role: "admin", ViaAdminAccess: true}
	stranger := &Principal{UserID: "other", Email: "other@example.net", Role: "user"}
	added := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	mk := func(p *Principal) (*API, *fakeRepo) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		repo.allowEntries["survival"] = []AllowlistEntry{
			{MCUUID: friend, Username: "Steve", AddedAt: added, CanWake: true},
			{MCUUID: other, AddedAt: added.Add(-time.Hour), CanWake: false},
		}
		a := newTestAPI(repo, newFakeCluster())
		a.External = staticExternal{p: p}
		return a, repo
	}
	list := func(t *testing.T, a *API) []AllowlistEntry {
		t.Helper()
		w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/allowlist", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("list: code = %d (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Server  string           `json:"server"`
			Entries []AllowlistEntry `json:"entries"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("list: bad JSON: %v", err)
		}
		if resp.Server != "survival" {
			t.Fatalf("list: server = %q", resp.Server)
		}
		return resp.Entries
	}

	for _, tc := range []struct {
		label string
		p     *Principal
	}{{"owner", owner}, {"admin", admin}} {
		t.Run(tc.label+" lists and revokes", func(t *testing.T) {
			a, repo := mk(tc.p)
			got := list(t, a)
			if len(got) != 2 || got[0].MCUUID != friend || got[0].Username != "Steve" || !got[0].CanWake ||
				got[1].MCUUID != other || got[1].CanWake || !got[0].AddedAt.Equal(added) {
				t.Fatalf("entries = %+v", got)
			}

			// An upper-case UUID names the same entry: the route canonicalizes it.
			w := do(a.ExternalHandler(), "PUT", "/api/v1/servers/survival/allowlist/"+strings.ToUpper(friend),
				`{"can_wake":false}`, jsonHeader)
			if w.Code != http.StatusNoContent {
				t.Fatalf("revoke: code = %d (%s)", w.Code, w.Body.String())
			}
			if list(t, a)[0].CanWake {
				t.Fatal("revoke left the wake right in place")
			}
			if len(repo.audits) != 1 || repo.audits[0].Action != "allowlist.revoke" ||
				repo.audits[0].ServerName != "survival" || repo.audits[0].ActorUserID != tc.p.UserID ||
				!strings.Contains(string(repo.audits[0].Payload), friend) {
				t.Fatalf("revoke audit = %+v", repo.audits)
			}

			w = do(a.ExternalHandler(), "PUT", "/api/v1/servers/survival/allowlist/"+other,
				`{"can_wake":true}`, jsonHeader)
			if w.Code != http.StatusNoContent {
				t.Fatalf("restore: code = %d (%s)", w.Code, w.Body.String())
			}
			if !list(t, a)[1].CanWake {
				t.Fatal("restore did not give the wake right back")
			}
			if len(repo.audits) != 2 || repo.audits[1].Action != "allowlist.restore" {
				t.Fatalf("restore audit = %+v", repo.audits)
			}
		})
	}

	t.Run("stranger is refused on both routes", func(t *testing.T) {
		a, repo := mk(stranger)
		if w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/allowlist", "", nil); w.Code != http.StatusForbidden {
			t.Fatalf("list: code = %d, want 403", w.Code)
		}
		w := do(a.ExternalHandler(), "PUT", "/api/v1/servers/survival/allowlist/"+friend, `{"can_wake":false}`, jsonHeader)
		if w.Code != http.StatusForbidden {
			t.Fatalf("revoke: code = %d, want 403", w.Code)
		}
		if !repo.allowEntries["survival"][0].CanWake || len(repo.audits) != 0 {
			t.Fatalf("a refused revoke changed something: %+v, audits %+v", repo.allowEntries["survival"], repo.audits)
		}
	})

	t.Run("unknown server, unknown entry and bad input", func(t *testing.T) {
		a, repo := mk(owner)
		for _, c := range []struct {
			method, path, body string
			want               int
		}{
			{"GET", "/api/v1/servers/nowhere/allowlist", "", http.StatusNotFound},
			{"PUT", "/api/v1/servers/nowhere/allowlist/" + friend, `{"can_wake":false}`, http.StatusNotFound},
			{"PUT", "/api/v1/servers/survival/allowlist/11111111-2222-3333-4444-555555555555", `{"can_wake":false}`, http.StatusNotFound},
			{"PUT", "/api/v1/servers/survival/allowlist/not-a-uuid", `{"can_wake":false}`, http.StatusBadRequest},
			{"PUT", "/api/v1/servers/survival/allowlist/" + friend, `{}`, http.StatusBadRequest},
			{"GET", "/api/v1/servers/Bad_Name/allowlist", "", http.StatusBadRequest},
		} {
			if w := do(a.ExternalHandler(), c.method, c.path, c.body, jsonHeader); w.Code != c.want {
				t.Errorf("%s %s %s: code = %d, want %d (%s)", c.method, c.path, c.body, w.Code, c.want, w.Body.String())
			}
		}
		if !repo.allowEntries["survival"][0].CanWake || len(repo.audits) != 0 {
			t.Fatalf("a failed call changed something: %+v, audits %+v", repo.allowEntries["survival"], repo.audits)
		}

		repo.allowlistErr = errors.New("db down")
		w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/allowlist", "", nil)
		if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "db down") {
			t.Fatalf("a failed read: code = %d (%s), want an opaque 500", w.Code, w.Body.String())
		}
	})
}
