package api

import (
	"net/http"
	"testing"
)

// statusPath builds the internal link-status poll path for a UUID.
func statusPath(mcUUID string) string {
	return "/api/v1/internal/account/link/status/" + mcUUID
}

func TestQRLoginCompletionPollVertical(t *testing.T) {
	const mcUUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	user := &Principal{UserID: "u-scan", Email: "scan@example.net", Role: "user"}

	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: user}
	ih := api.InternalHandler()
	eh := api.ExternalHandler()

	w := do(ih, "GET", statusPath(mcUUID), "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pre-scan poll: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if b := acctBody(t, w); b["linked"] != false {
		t.Fatalf("pre-scan poll body = %v, want linked:false", b)
	} else if _, ok := b["user_id"]; ok {
		t.Fatalf("pre-scan poll leaked user_id: %v", b)
	}

	// The QR encodes a one-time code velocity mints in-game (internal face).
	w = do(ih, "POST", "/api/v1/internal/account/link/code", `{"mc_uuid":"`+mcUUID+`"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint code: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	code, _ := acctBody(t, w)["code"].(string)

	// The player scans it on a phone already signed in to the panel: that web
	// session's verify writes the durable link, bound to THAT Principal (external).
	w = do(eh, "POST", "/api/v1/account/link/verify", `{"code":"`+code+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// Now the poll flips: velocity sees linked:true and admits the player. The
	// response stays identity-free — linked is the entire contract.
	w = do(ih, "GET", statusPath(mcUUID), "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("post-verify poll: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	b := acctBody(t, w)
	if b["linked"] != true {
		t.Fatalf("post-verify poll body = %v, want linked:true", b)
	}
	if _, ok := b["user_id"]; ok {
		t.Fatalf("post-verify poll leaked user_id: %v", b)
	}
}

func TestQRLoginStatusUnknownUUID(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	w := do(api.InternalHandler(), "GET", statusPath("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"), "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("unknown-uuid poll: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if b := acctBody(t, w); b["linked"] != false {
		t.Fatalf("unknown-uuid poll body = %v, want linked:false", b)
	}
}

func TestQRLoginStatusIdempotent(t *testing.T) {
	const mcUUID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	repo := newFakeRepo()
	repo.links[mcUUID] = "u-held" // already linked
	api := newTestAPI(repo, newFakeCluster())
	ih := api.InternalHandler()

	for i := 0; i < 2; i++ {
		w := do(ih, "GET", statusPath(mcUUID), "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("poll %d: code = %d, want 200 (%s)", i, w.Code, w.Body.String())
		}
		b := acctBody(t, w)
		if b["linked"] != true {
			t.Fatalf("poll %d body = %v, want linked:true", i, b)
		}
	}
	// The read must not have disturbed the durable link.
	if repo.links[mcUUID] != "u-held" {
		t.Errorf("poll consumed the link: links[%s] = %q, want u-held", mcUUID, repo.links[mcUUID])
	}
}

func TestQRLoginStatusFaceSeparation(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: user}
	if w := do(api.ExternalHandler(), "GET", statusPath("dddddddd-dddd-dddd-dddd-dddddddddddd"), "", nil); w.Code != http.StatusNotFound {
		t.Errorf("status endpoint on external face: code = %d, want 404", w.Code)
	}
}
