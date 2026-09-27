package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Every door that takes an mc_uuid refuses text that is not a UUID with 400
// bad_mc_uuid. The columns are Postgres uuids, and such text used to reach the
// database, fail there with 22P02 and come back as a 500. A UUID in another
// spelling is stored and echoed in the one Postgres gives back.
func TestMCUUIDMustBeAUUID(t *testing.T) {
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "u1"}
	repo.claimOK["survival"] = true
	repo.seedUser(UserView{ID: "u2", Username: "alice", Role: "user"})
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", AutostartPolicy: "public"}
	api := newTestAPI(repo, cl)
	api.External = staticExternal{p: &Principal{UserID: "owner1", Role: "owner", ViaAdminAccess: true}}
	ih, eh := api.InternalHandler(), api.ExternalHandler()

	const bad = "not-a-uuid"
	body := `{"mc_uuid":"` + bad + `"}`
	for _, c := range []struct {
		h                  http.Handler
		method, path, body string
	}{
		{ih, "POST", "/api/v1/internal/servers/survival/join-event", body},
		{ih, "POST", "/api/v1/internal/servers/survival/wake", body},
		{ih, "POST", "/api/v1/internal/servers/survival/claim", body},
		{ih, "GET", "/api/v1/internal/player/menu-access/" + bad, ""},
		{ih, "POST", "/api/v1/internal/account/link/code", body},
		{ih, "GET", "/api/v1/internal/account/link/status/" + bad, ""},
		{ih, "POST", "/api/v1/internal/account/migrate/start", body},
		{ih, "GET", "/api/v1/internal/player/blacklist/" + bad, ""},
		{eh, "PUT", "/api/v1/servers/survival/allowlist/" + bad, `{"can_wake":true}`},
		{eh, "DELETE", "/api/v1/users/u2/links/" + bad, ""},
		{eh, "POST", "/api/v1/users/u2/links", body},
	} {
		w := do(c.h, c.method, c.path, c.body, jsonHeader)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_mc_uuid" {
			t.Errorf("%s %s with %q: code = %d body %s, want 400 bad_mc_uuid", c.method, c.path, bad, w.Code, w.Body.String())
		}
	}
	if len(repo.joins) != 0 || len(repo.linkCodes) != 0 || len(repo.links) != 0 || len(repo.audits) != 0 {
		t.Fatalf("a refused mc_uuid wrote joins=%v codes=%v links=%v audits=%v", repo.joins, repo.linkCodes, repo.links, repo.audits)
	}

	const spelled, canonical = " 069A79F444E94726A5BEFCA90E38AAF5 ", "069a79f4-44e9-4726-a5be-fca90e38aaf5"
	w := do(eh, "POST", "/api/v1/users/u2/links", `{"mc_uuid":"`+spelled+`"}`, jsonHeader)
	var linked struct {
		MCUUID string `json:"mc_uuid"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &linked); err != nil || w.Code != http.StatusOK || linked.MCUUID != canonical {
		t.Fatalf("link of %q: code = %d body %s, want 200 echoing %s", spelled, w.Code, w.Body.String(), canonical)
	}
	if repo.links[canonical] != "u2" || len(repo.links) != 1 {
		t.Fatalf("links = %v, want only %s → u2", repo.links, canonical)
	}
	if w := do(ih, "POST", "/api/v1/internal/account/link/code", `{"mc_uuid":"`+spelled+`"}`, jsonHeader); w.Code != http.StatusCreated {
		t.Fatalf("link code for %q: code = %d body %s, want 201", spelled, w.Code, w.Body.String())
	}
	for _, lc := range repo.linkCodes {
		if lc.mcUUID != canonical {
			t.Fatalf("link code minted for %q, want %s", lc.mcUUID, canonical)
		}
	}
}
