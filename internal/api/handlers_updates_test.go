package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Platform maintenance handlers require Owner access through the operator host.
// Handler tests use an Owner; boundary tests vary the principal explicitly.
func seedUpdatesAPI(t *testing.T) (*API, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{
		UserID: "owner1", Email: "owner@" + testRoot, Role: "owner", ViaAdminAccess: true,
	}}
	return api, repo
}

// decodeWindow reads the {start,end} body, each a string or nil.
func decodeWindow(t *testing.T, w interface{ Bytes() []byte }) (start, end any) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Bytes(), &body); err != nil {
		t.Fatalf("window body not JSON: %v", err)
	}
	if _, ok := body["start"]; !ok {
		t.Fatalf("window body missing start key: %s", string(w.Bytes()))
	}
	if _, ok := body["end"]; !ok {
		t.Fatalf("window body missing end key: %s", string(w.Bytes()))
	}
	return body["start"], body["end"]
}

// TestUpdateWindowUnsetReturnsNulls: a never-set window reads back as {null,null}
// (ErrNotFound → 200 nulls), so the client has one shape whether or not a window
// was ever configured.
func TestUpdateWindowUnsetReturnsNulls(t *testing.T) {
	api, _ := seedUpdatesAPI(t)
	w := do(api.ExternalHandler(), "GET", "/api/v1/updates/window", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if start, end := decodeWindow(t, w.Body); start != nil || end != nil {
		t.Fatalf("unset window = {%v,%v}, want {null,null}", start, end)
	}
}

// TestUpdateWindowSetThenGet: a valid window is persisted and read back verbatim.
func TestUpdateWindowSetThenGet(t *testing.T) {
	api, repo := seedUpdatesAPI(t)
	const startISO, endISO = "2026-08-01T02:00:00Z", "2026-08-01T04:00:00Z"
	body := `{"start":"` + startISO + `","end":"` + endISO + `"}`

	put := do(api.ExternalHandler(), "PUT", "/api/v1/updates/window", body, jsonHeader)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT code = %d, want 200 (%s)", put.Code, put.Body.String())
	}
	if start, end := decodeWindow(t, put.Body); start != startISO || end != endISO {
		t.Fatalf("PUT echo = {%v,%v}, want {%s,%s}", start, end, startISO, endISO)
	}
	// It was actually persisted (not merely echoed).
	if _, ok := repo.settings[updateWindowKey]; !ok {
		t.Fatalf("window was not written to platform_settings[%q]", updateWindowKey)
	}

	get := do(api.ExternalHandler(), "GET", "/api/v1/updates/window", "", nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET code = %d, want 200 (%s)", get.Code, get.Body.String())
	}
	if start, end := decodeWindow(t, get.Body); start != startISO || end != endISO {
		t.Fatalf("GET after set = {%v,%v}, want {%s,%s}", start, end, startISO, endISO)
	}
}

// TestUpdateWindowClearRoundtrips: PUT {null,null} clears a previously set window,
// and the cleared window reads back as {null,null} — the SAME shape as never-set,
// exercising the second code path that yields nulls.
func TestUpdateWindowClearRoundtrips(t *testing.T) {
	api, _ := seedUpdatesAPI(t)
	set := do(api.ExternalHandler(), "PUT", "/api/v1/updates/window",
		`{"start":"2026-08-01T02:00:00Z","end":"2026-08-01T04:00:00Z"}`, jsonHeader)
	if set.Code != http.StatusOK {
		t.Fatalf("initial set code = %d, want 200 (%s)", set.Code, set.Body.String())
	}

	clear := do(api.ExternalHandler(), "PUT", "/api/v1/updates/window", `{"start":null,"end":null}`, jsonHeader)
	if clear.Code != http.StatusOK {
		t.Fatalf("clear code = %d, want 200 (%s)", clear.Code, clear.Body.String())
	}
	if start, end := decodeWindow(t, clear.Body); start != nil || end != nil {
		t.Fatalf("clear echo = {%v,%v}, want {null,null}", start, end)
	}

	get := do(api.ExternalHandler(), "GET", "/api/v1/updates/window", "", nil)
	if start, end := decodeWindow(t, get.Body); start != nil || end != nil {
		t.Fatalf("GET after clear = {%v,%v}, want {null,null}", start, end)
	}
}

// TestUpdateWindowRejectsMalformed: a half-set (exactly one end) or inverted/empty
// (end not after start) window is 400 and is NEVER written — the store can only
// ever hold a fully-valid or a fully-cleared window, mirroring the core's
// fail-closed Window.
func TestUpdateWindowRejectsMalformed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"start without end", `{"start":"2026-08-01T02:00:00Z","end":null}`},
		{"end without start", `{"start":null,"end":"2026-08-01T04:00:00Z"}`},
		{"inverted", `{"start":"2026-08-01T04:00:00Z","end":"2026-08-01T02:00:00Z"}`},
		{"empty interval", `{"start":"2026-08-01T02:00:00Z","end":"2026-08-01T02:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, repo := seedUpdatesAPI(t)
			w := do(api.ExternalHandler(), "PUT", "/api/v1/updates/window", tc.body, jsonHeader)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			if code := decodeErr(t, w); code != "bad_request" {
				t.Fatalf("error code = %q, want bad_request", code)
			}
			if _, ok := repo.settings[updateWindowKey]; ok {
				t.Fatal("a rejected window must not be persisted")
			}
		})
	}
}

// TestUpdateWindowContentTypeGuard pins the cross-site-forgery guard on the mutating
// PUT: a body an HTML form could emit is rejected 415 before any write.
func TestUpdateWindowContentTypeGuard(t *testing.T) {
	api, repo := seedUpdatesAPI(t)
	w := do(api.ExternalHandler(), "PUT", "/api/v1/updates/window",
		`{"start":"2026-08-01T02:00:00Z","end":"2026-08-01T04:00:00Z"}`,
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("code = %d, want 415 (%s)", w.Code, w.Body.String())
	}
	if _, ok := repo.settings[updateWindowKey]; ok {
		t.Fatal("a content-type-rejected PUT must not persist")
	}
}

// Verify the Owner and operator-host boundaries for reads and writes.
func TestUpdateWindowOwnerGating(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *Principal
		want int
	}{
		{"owner via operator host", &Principal{UserID: "o1", Role: "owner", ViaAdminAccess: true}, http.StatusOK},
		{"owner off operator host", &Principal{UserID: "o1", Role: "owner", ViaAdminAccess: false}, http.StatusForbidden},
		{"admin via admin-access", &Principal{UserID: "a1", Role: "admin", ViaAdminAccess: true}, http.StatusForbidden},
		{"admin off operator host", &Principal{UserID: "a1", Role: "admin", ViaAdminAccess: false}, http.StatusForbidden},
		{"role=user player", &Principal{UserID: "u1", Role: "user", ViaAdminAccess: false}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			api := newTestAPI(repo, newFakeCluster())
			api.External = staticExternal{p: tc.p}

			for _, path := range []string{"/api/v1/updates/window", "/api/v1/updates/report", "/api/v1/platform/db-backup"} {
				get := do(api.ExternalHandler(), "GET", path, "", nil)
				if get.Code != tc.want {
					t.Fatalf("GET %s code = %d, want %d (%s)", path, get.Code, tc.want, get.Body.String())
				}
			}
			// A valid PUT by an Owner is 200; other identities are rejected
			// before the handler), so the wanted PUT code is the same as the GET's.
			put := do(api.ExternalHandler(), "PUT", "/api/v1/updates/window",
				`{"start":"2026-08-01T02:00:00Z","end":"2026-08-01T04:00:00Z"}`, jsonHeader)
			if put.Code != tc.want {
				t.Fatalf("PUT code = %d, want %d (%s)", put.Code, tc.want, put.Body.String())
			}
		})
	}
}
