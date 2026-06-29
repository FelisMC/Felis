package api

import (
	"net/http"
	"testing"
)

// statusPath builds the internal link-status poll path for a UUID.
func statusPath(mcUUID string) string {
	return "/api/v1/internal/account/link/status/" + mcUUID
}

// TestQRLoginCompletionPollVertical walks the QR scan-to-login flow end to end and
// proves its load-bearing invariant: the internal completion poll reports the link
// only after the WEB verify writes it, and reports it bound to the exact Principal
// that verified — never to a UUID the poll itself could name. velocity mints and
// polls on the internal face (it holds no web Principal); the durable bind is born
// on the external face from a logged-in user. That split is the whole security
// model of the scan, so the test drives both faces of one API.
func TestQRLoginCompletionPollVertical(t *testing.T) {
	const mcUUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	user := &Principal{UserID: "u-scan", Email: "scan@example.net", Role: "user"}

	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: user}
	ih := api.InternalHandler()
	eh := api.ExternalHandler()

	// Before the player scans, velocity is already polling the UUID it minted
	// against. The link does not exist yet, so the poll says "keep waiting" — a
	// plain 200 linked:false, NOT an error, and with no user_id to leak.
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

	// Now the poll flips: velocity sees linked:true and the user_id it must bind the
	// in-game session to — and that user_id is the verifier's, the only identity the
	// poll could ever return, since the poll cannot mint a link of its own.
	w = do(ih, "GET", statusPath(mcUUID), "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("post-verify poll: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	b := acctBody(t, w)
	if b["linked"] != true {
		t.Fatalf("post-verify poll body = %v, want linked:true", b)
	}
	if got := b["user_id"]; got != user.UserID {
		t.Fatalf("post-verify poll user_id = %v, want %q (the verifier's id)", got, user.UserID)
	}
}

// TestQRLoginStatusUnknownUUID pins that a UUID the platform has never linked is
// reported as not-linked, not as an error. velocity polls public online-mode UUIDs;
// an unknown one means "do not admit yet", indistinguishable by design from a code
// that has simply not been scanned.
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

// TestQRLoginStatusIdempotent re-polls a linked UUID and expects the identical
// answer: the poll is a pure read of the durable link, so a velocity that restarts
// mid-handshake and re-polls must never get a different verdict or consume the link.
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
		if b["linked"] != true || b["user_id"] != "u-held" {
			t.Fatalf("poll %d body = %v, want linked:true user_id:u-held", i, b)
		}
	}
	// The read must not have disturbed the durable link.
	if repo.links[mcUUID] != "u-held" {
		t.Errorf("poll consumed the link: links[%s] = %q, want u-held", mcUUID, repo.links[mcUUID])
	}
}

// TestQRLoginStatusFaceSeparation enforces that the poll is internal-only. It
// reads who a UUID is linked to — a fact the public web face must not be able to
// fish out by UUID — so crossing onto the external face must 404, not answer.
func TestQRLoginStatusFaceSeparation(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: user}
	if w := do(api.ExternalHandler(), "GET", statusPath("dddddddd-dddd-dddd-dddd-dddddddddddd"), "", nil); w.Code != http.StatusNotFound {
		t.Errorf("status endpoint on external face: code = %d, want 404", w.Code)
	}
}
