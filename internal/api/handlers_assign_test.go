package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

const assignPath = "/api/v1/servers/survival/assignment"

func assignAPI(p *Principal) (*API, *fakeRepo) {
	repo := newFakeRepo()
	repo.owners["survival"] = ServerOwnership{OwnerID: "alice"}
	a := newTestAPI(repo, claimCluster())
	a.External = staticExternal{p: p}
	return a, repo
}

func TestAssignmentIsOwnerOnly(t *testing.T) {
	a, repo := assignAPI(&Principal{UserID: "admin", Role: "admin", ViaAdminAccess: true})
	if w := do(a.ExternalHandler(), "GET", assignPath+"?user_id=bob", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("admin preview = %d", w.Code)
	}
	if w := do(a.ExternalHandler(), "POST", assignPath, `{"user_id":"bob"}`, jsonHeader); w.Code != http.StatusForbidden {
		t.Fatalf("admin assign = %d", w.Code)
	}
	if repo.owners["survival"].OwnerID != "alice" {
		t.Fatal("a refused assignment changed the owner")
	}
}

func TestAssignmentOverQuotaNeedsConfirmation(t *testing.T) {
	a, repo := assignAPI(&Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true})
	repo.assignOver = []QuotaExcess{{Resource: "servers", Limit: 1, After: 2}}

	preview := do(a.ExternalHandler(), "GET", assignPath+"?user_id=bob", "", nil)
	var view assignmentView
	if err := json.Unmarshal(preview.Body.Bytes(), &view); err != nil || view.OwnerID != "alice" || len(view.Over) != 1 || view.Over[0].After != 2 {
		t.Fatalf("preview = %d %s", preview.Code, preview.Body.String())
	}

	w := do(a.ExternalHandler(), "POST", assignPath, `{"user_id":"bob"}`, jsonHeader)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "quota_exceeded" || repo.owners["survival"].OwnerID != "alice" {
		t.Fatalf("unconfirmed assign = %d %s", w.Code, w.Body.String())
	}

	w = do(a.ExternalHandler(), "POST", assignPath, `{"user_id":"bob","allow_over_quota":true}`, jsonHeader)
	if w.Code != http.StatusOK || repo.owners["survival"].OwnerID != "bob" {
		t.Fatalf("confirmed assign = %d %s", w.Code, w.Body.String())
	}
	last := repo.audits[len(repo.audits)-1]
	var payload struct {
		From  string        `json:"owner_from"`
		To    string        `json:"owner_to"`
		Quota []QuotaExcess `json:"over_quota"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil || last.Action != "server.assign" ||
		payload.From != "alice" || payload.To != "bob" || len(payload.Quota) != 1 {
		t.Fatalf("audit = %+v %s", last, last.Payload)
	}
}

func TestAssignmentRefusals(t *testing.T) {
	a, repo := assignAPI(&Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true})
	for _, tc := range []struct {
		path, body string
		err        error
		status     int
		code       string
	}{
		{assignPath, `{}`, nil, http.StatusBadRequest, "bad_request"},
		{"/api/v1/servers/lobby/assignment", `{"user_id":"bob"}`, nil, http.StatusBadRequest, "bad_name"},
		{assignPath, `{"user_id":"ghost"}`, ErrUnknownAssignee, http.StatusBadRequest, "unknown_user"},
		{assignPath, `{"user_id":"alice"}`, ErrAlreadyOwner, http.StatusConflict, "already_owner"},
		{assignPath, `{"user_id":"bob"}`, ErrAssignRetiring, http.StatusConflict, "server_retiring"},
	} {
		repo.assignErr = tc.err
		w := do(a.ExternalHandler(), "POST", tc.path, tc.body, jsonHeader)
		if w.Code != tc.status || decodeErr(t, w) != tc.code {
			t.Errorf("POST %s %s = %d %s, want %d %s", tc.path, tc.body, w.Code, w.Body.String(), tc.status, tc.code)
		}
	}
}
