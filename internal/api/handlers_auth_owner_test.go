package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const ownerStatusPath = "/api/v1/auth/owner-status"

func ownerBoundOf(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var v struct {
		OwnerBound *bool `json:"owner_bound"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.OwnerBound == nil {
		t.Fatalf("body = %s, want {\"owner_bound\": bool} (err %v)", w.Body.String(), err)
	}
	return *v.OwnerBound
}

// TestOwnerStatusReportsAnUnclaimedInstall pins what the sign-in page reads: false before
// any staff account exists, true once one does, and a player account alone is not an
// Owner. It answers with local auth still off, which is the state it exists to explain.
func TestOwnerStatusReportsAnUnclaimedInstall(t *testing.T) {
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	if localAuthEnabled(t.Context(), repo) {
		t.Fatal("precondition: a fresh fake must have local auth off")
	}
	if ownerBoundOf(t, do(api.ExternalHandler(), "GET", ownerStatusPath, "", nil)) {
		t.Error("an install with no accounts reports an Owner")
	}

	repo.staff["player"] = &StaffUser{ID: "u1", Username: "player", Role: "user"}
	if ownerBoundOf(t, do(api.ExternalHandler(), "GET", ownerStatusPath, "", nil)) {
		t.Error("a player account alone reports an Owner")
	}

	repo.staff["boss"] = &StaffUser{ID: "o1", Username: "boss", Role: "owner"}
	if !ownerBoundOf(t, do(api.ExternalHandler(), "GET", ownerStatusPath, "", nil)) {
		t.Error("an install with an Owner reports none")
	}
}

// TestOwnerStatusCachesTheBoundAnswer pins that once an Owner is seen the probe stops
// querying: a store that then fails still gets the cached true.
func TestOwnerStatusCachesTheBoundAnswer(t *testing.T) {
	repo := newFakeRepo()
	repo.staff["boss"] = &StaffUser{ID: "o1", Username: "boss", Role: "owner"}
	api := newTestAPI(repo, newFakeCluster())
	if !ownerBoundOf(t, do(api.ExternalHandler(), "GET", ownerStatusPath, "", nil)) {
		t.Fatal("an install with an Owner reports none")
	}
	repo.failAdminExists = errors.New("store down")
	if !ownerBoundOf(t, do(api.ExternalHandler(), "GET", ownerStatusPath, "", nil)) {
		t.Error("the bound answer was not cached")
	}
}

// TestOwnerStatusStoreFailure pins that an outage is a 503, never a false "no Owner":
// the page must fall back to its doors rather than tell a claimed install to run setup.
// An unbound answer is not cached either, so the next call reads the store again.
func TestOwnerStatusStoreFailure(t *testing.T) {
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	repo.failAdminExists = errors.New("store down")
	w := do(api.ExternalHandler(), "GET", ownerStatusPath, "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	if got := decodeErr(t, w); got != "auth_unavailable" {
		t.Errorf("error = %q, want auth_unavailable", got)
	}

	repo.failAdminExists = nil
	if ownerBoundOf(t, do(api.ExternalHandler(), "GET", ownerStatusPath, "", nil)) {
		t.Error("an install with no accounts reports an Owner")
	}
	repo.staff["boss"] = &StaffUser{ID: "o1", Username: "boss", Role: "owner"}
	if !ownerBoundOf(t, do(api.ExternalHandler(), "GET", ownerStatusPath, "", nil)) {
		t.Error("an unbound answer was cached past the Owner's arrival")
	}
}
