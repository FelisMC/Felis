package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// Passkey enrollment handler tests (spec §14 / Phase 6 bind), enrollment-only slice.
// They drive the four account routes against the fakeRepo state machine and a fake
// PasskeyVerifier — no real attestation crypto, no SQL — so what these PROVE is the
// handler + challenge state machine, not the pgrepo SQL (mirrored, not run here) nor
// the cryptographic verification (Slice 1).

// frozenNow is the test clock newTestAPI installs; a live challenge expires at
// frozenNow+passkeyChallengeTTL, an expired one strictly before frozenNow.
var frozenNow = time.Unix(1_700_000_000, 0)

// newPasskeyAPI wires an external face with a fake verifier and a fixed principal.
func newPasskeyAPI(repo *fakeRepo, v PasskeyVerifier, p *Principal) http.Handler {
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: p}
	api.Passkey = v
	return api.ExternalHandler()
}

// plantPasskeyChallenge seeds a stashed registration challenge for u1 directly, so the
// finish-side branches (expired, conflict) are reachable under the frozen clock without
// running begin first.
func plantPasskeyChallenge(repo *fakeRepo, id string, expiresAt time.Time, session []byte) {
	repo.passkeyChallenges[id] = &fakePasskeyChallenge{
		id: id, userID: "u1", purpose: passkeyPurposeRegister,
		sessionData: session, expiresAt: expiresAt, createdAt: expiresAt,
	}
}

// TestPasskeyRegisterVertical walks the whole enrollment slice across the external
// face: begin mints + stashes a challenge, finish verifies the attestation against the
// SERVER-STASHED session data and binds the credential, list shows it, delete unbinds
// it. The decisive assertion is the session-data round-trip: the finish body carries
// only name+attestation, so the only path for the stashed blob into FinishRegistration
// is store-stash → consume — proving the challenge is never client-echoed.
func TestPasskeyRegisterVertical(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	v := &fakePasskeyVerifier{
		options: json.RawMessage(`{"publicKey":{"challenge":"Y2hhbGxlbmdl"}}`),
		credential: VerifiedCredential{
			CredentialID: "cred-abc", PublicKey: "SECRET_COSE_KEY", SignCount: 0, AAGUID: "aaguid-1",
		},
	}
	eh := newPasskeyAPI(repo, v, user)

	// 1) begin returns the creation options FLAT (envelope stripped for the panel) and
	// stashes exactly one challenge bound to the caller.
	w := do(eh, "POST", "/api/v1/account/passkey/register/begin", `{}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if b := acctBody(t, w); b["challenge"] == nil || b["publicKey"] != nil {
		t.Errorf("begin must return FLAT creation options (top-level challenge, no publicKey envelope), got %s", w.Body.String())
	}
	if len(repo.passkeyChallenges) != 1 {
		t.Fatalf("begin must stash exactly one challenge, got %d", len(repo.passkeyChallenges))
	}
	if string(v.lastUser.ID) != "u1" {
		t.Errorf("begin passed user id %q, want u1", v.lastUser.ID)
	}

	// 2) finish binds the credential. The finish body carries NO challenge — only the
	// nickname and attestation.
	body := `{"name":"My YubiKey","attestation":{"id":"abc","type":"public-key"}}`
	w = do(eh, "POST", "/api/v1/account/passkey/register/finish", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("finish: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	// THE security assertion: the blob the verifier saw at finish is exactly what begin
	// stashed — it travelled store-stash → consume, never the client.
	if !bytes.Equal(v.lastSession, []byte("session:u1")) {
		t.Fatalf("finish session data = %q, want the server-stashed %q (challenge must not be client-echoed)",
			v.lastSession, "session:u1")
	}
	// The view never leaks the public key.
	if bytes.Contains(w.Body.Bytes(), []byte("SECRET_COSE_KEY")) {
		t.Error("finish response leaked the credential public key")
	}
	fb := acctBody(t, w)
	if fb["name"] != "My YubiKey" {
		t.Errorf("finish view name = %v, want \"My YubiKey\"", fb["name"])
	}
	if len(repo.passkeyCreds) != 1 {
		t.Fatalf("finish must persist exactly one credential, got %d", len(repo.passkeyCreds))
	}

	// 3) the challenge is single-use: a second finish (no new begin) → 400.
	if w := do(eh, "POST", "/api/v1/account/passkey/register/finish", body, nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_challenge_invalid" {
		t.Fatalf("replayed finish: code = %d body %s, want 400 passkey_challenge_invalid", w.Code, w.Body.String())
	}

	// 4) list shows the bound credential (display fields only, never the public key).
	w = do(eh, "GET", "/api/v1/account/passkey/credentials", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("SECRET_COSE_KEY")) {
		t.Error("list response leaked the credential public key")
	}
	creds, _ := acctBody(t, w)["credentials"].([]any)
	if len(creds) != 1 {
		t.Fatalf("list returned %d credentials, want 1 (%s)", len(creds), w.Body.String())
	}
	id, _ := creds[0].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("listed credential has no id: %s", w.Body.String())
	}

	// 5) delete unbinds it (204) and the row is gone.
	if w := do(eh, "DELETE", "/api/v1/account/passkey/credentials/"+id, "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: code = %d, want 204 (%s)", w.Code, w.Body.String())
	}
	if len(repo.passkeyCreds) != 0 {
		t.Fatalf("delete left %d credentials, want 0", len(repo.passkeyCreds))
	}

	// Both mutating halves audit by the caller's Access email AND name the affected
	// credential id as the target, so an operator reading the log can tell which
	// passkey was bound/unbound (register previously logged an empty target).
	var registered, removed bool
	for _, a := range repo.audits {
		switch a.Action {
		case "account.passkey.registered":
			registered = a.Actor == "u1@example.net" && a.ServerName == id
		case "account.passkey.removed":
			removed = a.Actor == "u1@example.net" && a.ServerName == id
		}
	}
	if !registered || !removed {
		t.Errorf("want registered+removed audits by u1@example.net targeting %s, got %+v", id, repo.audits)
	}
}

// TestPasskeyBeginPassesExistingCredentials proves excludeCredentials is wired
// end-to-end: a caller who already has a passkey hands that credential to the verifier
// at begin, so the authenticator can refuse to double-bind one device. The hook is
// PasskeyCredentialsForUser → passkeyUserFor → BeginRegistration.
func TestPasskeyBeginPassesExistingCredentials(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	repo.passkeyCreds["row1"] = PasskeyCredential{
		ID: "row1", UserID: "u1", CredentialID: "existing-cred", PublicKey: "k", CreatedAt: frozenNow,
	}
	v := &fakePasskeyVerifier{}
	eh := newPasskeyAPI(repo, v, user)

	if w := do(eh, "POST", "/api/v1/account/passkey/register/begin", `{}`, nil); w.Code != http.StatusOK {
		t.Fatalf("begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(v.lastUser.Credentials) != 1 || v.lastUser.Credentials[0].CredentialID != "existing-cred" {
		t.Fatalf("begin must hand the caller's existing credentials to the verifier, got %+v", v.lastUser.Credentials)
	}
}

// TestPasskeyBeginSupersedes proves a fresh begin invalidates the prior in-flight
// challenge for the same user: two begins leave exactly one stashed challenge, so an
// abandoned ceremony cannot be finished after the user restarts.
func TestPasskeyBeginSupersedes(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	eh := newPasskeyAPI(repo, &fakePasskeyVerifier{}, user)

	do(eh, "POST", "/api/v1/account/passkey/register/begin", `{}`, nil)
	do(eh, "POST", "/api/v1/account/passkey/register/begin", `{}`, nil)
	if len(repo.passkeyChallenges) != 1 {
		t.Fatalf("a second begin must supersede the first; stashed challenges = %d, want 1", len(repo.passkeyChallenges))
	}
}

// TestPasskeyUnavailable pins the graceful-degradation path: when no verifier is wired
// the ceremony routes answer 503 (not a panic). It is NOT an auth assertion — auth runs
// in middleware upstream of these handlers regardless. List/delete need no verifier, so
// they keep working.
func TestPasskeyUnavailable(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	eh := newPasskeyAPI(repo, nil, user) // nil verifier

	if w := do(eh, "POST", "/api/v1/account/passkey/register/begin", `{}`, nil); w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "passkey_unavailable" {
		t.Fatalf("begin with no verifier: code = %d body %s, want 503 passkey_unavailable", w.Code, w.Body.String())
	}
	if w := do(eh, "POST", "/api/v1/account/passkey/register/finish", `{"name":"x","attestation":{"a":1}}`, nil); w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "passkey_unavailable" {
		t.Fatalf("finish with no verifier: code = %d body %s, want 503 passkey_unavailable", w.Code, w.Body.String())
	}
	// list still works without a verifier (it touches only the store).
	if w := do(eh, "GET", "/api/v1/account/passkey/credentials", "", nil); w.Code != http.StatusOK {
		t.Errorf("list with no verifier: code = %d, want 200 (it needs no verifier)", w.Code)
	}
}

// TestPasskeyFinishRejections is the finish-side failure matrix. The expired and
// conflict cases plant state directly: the test clock is frozen, so a pre-expired
// challenge or a pre-bound credential is the only way to reach those branches.
func TestPasskeyFinishRejections(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	const goodBody = `{"name":"k","attestation":{"id":"abc"}}`

	t.Run("missing attestation -> 400 bad_request", func(t *testing.T) {
		repo := newFakeRepo()
		eh := newPasskeyAPI(repo, &fakePasskeyVerifier{}, user)
		plantPasskeyChallenge(repo, "c1", frozenNow.Add(passkeyChallengeTTL), []byte("s"))
		if w := do(eh, "POST", "/api/v1/account/passkey/register/finish", `{"name":"k"}`, nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("code = %d body %s, want 400 bad_request", w.Code, w.Body.String())
		}
	})

	t.Run("unknown field -> 400 (strict decode)", func(t *testing.T) {
		repo := newFakeRepo()
		eh := newPasskeyAPI(repo, &fakePasskeyVerifier{}, user)
		if w := do(eh, "POST", "/api/v1/account/passkey/register/finish", `{"name":"k","attestation":{"a":1},"x":1}`, nil); w.Code != http.StatusBadRequest {
			t.Fatalf("strict decode must reject unknown field, code = %d", w.Code)
		}
	})

	t.Run("no live challenge -> 400 passkey_challenge_invalid", func(t *testing.T) {
		repo := newFakeRepo()
		eh := newPasskeyAPI(repo, &fakePasskeyVerifier{}, user)
		if w := do(eh, "POST", "/api/v1/account/passkey/register/finish", goodBody, nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_challenge_invalid" {
			t.Fatalf("code = %d body %s, want 400 passkey_challenge_invalid", w.Code, w.Body.String())
		}
	})

	t.Run("expired challenge -> 400 passkey_challenge_invalid, not consumed", func(t *testing.T) {
		repo := newFakeRepo()
		eh := newPasskeyAPI(repo, &fakePasskeyVerifier{}, user)
		plantPasskeyChallenge(repo, "ex", frozenNow.Add(-time.Second), []byte("s"))
		if w := do(eh, "POST", "/api/v1/account/passkey/register/finish", goodBody, nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_challenge_invalid" {
			t.Fatalf("code = %d body %s, want 400 passkey_challenge_invalid", w.Code, w.Body.String())
		}
		if repo.passkeyChallenges["ex"].consumed {
			t.Error("an expired challenge must not be consumed")
		}
	})

	t.Run("attestation fails verification -> 400 invalid_attestation", func(t *testing.T) {
		repo := newFakeRepo()
		v := &fakePasskeyVerifier{failErr: errors.New("bad signature")}
		eh := newPasskeyAPI(repo, v, user)
		plantPasskeyChallenge(repo, "c1", frozenNow.Add(passkeyChallengeTTL), []byte("s"))
		if w := do(eh, "POST", "/api/v1/account/passkey/register/finish", goodBody, nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_attestation" {
			t.Fatalf("code = %d body %s, want 400 invalid_attestation", w.Code, w.Body.String())
		}
		// A bad attestation still consumes the challenge (single-use): the consume is
		// committed before verification, so a re-finish finds nothing live.
		if !repo.passkeyChallenges["c1"].consumed {
			t.Error("a consumed challenge must stay consumed even when verification fails")
		}
	})

	t.Run("credential already bound -> 409 passkey_already_bound", func(t *testing.T) {
		repo := newFakeRepo()
		// Some other account already holds this credential id.
		repo.passkeyCreds["other"] = PasskeyCredential{ID: "other", UserID: "u2", CredentialID: "dup-cred", CreatedAt: frozenNow}
		v := &fakePasskeyVerifier{credential: VerifiedCredential{CredentialID: "dup-cred", PublicKey: "k"}}
		eh := newPasskeyAPI(repo, v, user)
		plantPasskeyChallenge(repo, "c1", frozenNow.Add(passkeyChallengeTTL), []byte("s"))
		if w := do(eh, "POST", "/api/v1/account/passkey/register/finish", goodBody, nil); w.Code != http.StatusConflict || decodeErr(t, w) != "passkey_already_bound" {
			t.Fatalf("code = %d body %s, want 409 passkey_already_bound", w.Code, w.Body.String())
		}
	})
}

// TestPasskeyDeleteScoping proves a caller can only unbind their OWN passkey: u2's
// credential is invisible to u1's list and u1's delete of it 404s (never a silent
// success that would let one account strip another's factor).
func TestPasskeyDeleteScoping(t *testing.T) {
	u1 := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	repo.passkeyCreds["row2"] = PasskeyCredential{ID: "row2", UserID: "u2", CredentialID: "u2-cred", CreatedAt: frozenNow}
	eh := newPasskeyAPI(repo, &fakePasskeyVerifier{}, u1)

	// u1's list does not show u2's credential.
	w := do(eh, "GET", "/api/v1/account/passkey/credentials", "", nil)
	if creds, _ := acctBody(t, w)["credentials"].([]any); len(creds) != 0 {
		t.Fatalf("u1 list shows %d credentials, want 0 (u2's must be invisible)", len(creds))
	}
	// u1 cannot delete u2's credential.
	if w := do(eh, "DELETE", "/api/v1/account/passkey/credentials/row2", "", nil); w.Code != http.StatusNotFound || decodeErr(t, w) != "not_found" {
		t.Fatalf("cross-user delete: code = %d body %s, want 404 not_found", w.Code, w.Body.String())
	}
	if _, ok := repo.passkeyCreds["row2"]; !ok {
		t.Error("u2's credential must survive u1's failed delete")
	}
}

// TestPasskeyDeleteUnknown pins the unknown-id path: deleting an id that does not exist
// is a 404, never a silent 204.
func TestPasskeyDeleteUnknown(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	eh := newPasskeyAPI(newFakeRepo(), &fakePasskeyVerifier{}, user)
	if w := do(eh, "DELETE", "/api/v1/account/passkey/credentials/nope", "", nil); w.Code != http.StatusNotFound || decodeErr(t, w) != "not_found" {
		t.Fatalf("delete unknown: code = %d body %s, want 404 not_found", w.Code, w.Body.String())
	}
}

// TestPasskeyListNewestFirst proves the management view orders newest-first, matching
// the pgrepo ORDER BY created_at DESC the fake mirrors.
func TestPasskeyListNewestFirst(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	repo.passkeyCreds["old"] = PasskeyCredential{ID: "old", UserID: "u1", CredentialID: "c-old", Name: "old", CreatedAt: frozenNow.Add(-time.Hour)}
	repo.passkeyCreds["new"] = PasskeyCredential{ID: "new", UserID: "u1", CredentialID: "c-new", Name: "new", CreatedAt: frozenNow}
	eh := newPasskeyAPI(repo, &fakePasskeyVerifier{}, user)

	w := do(eh, "GET", "/api/v1/account/passkey/credentials", "", nil)
	creds, _ := acctBody(t, w)["credentials"].([]any)
	if len(creds) != 2 {
		t.Fatalf("list returned %d, want 2 (%s)", len(creds), w.Body.String())
	}
	if first, _ := creds[0].(map[string]any)["name"].(string); first != "new" {
		t.Errorf("list order: first = %q, want \"new\" (newest first)", first)
	}
}

// TestPasskeyFaceSeparation enforces that the enrollment routes are web-only: they
// require a logged-in principal the internal (service-token) face never carries, so
// crossing the face boundary must 404 rather than silently work.
func TestPasskeyFaceSeparation(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: user}
	api.Passkey = &fakePasskeyVerifier{}
	ih := api.InternalHandler()

	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/account/passkey/register/begin", `{}`},
		{"POST", "/api/v1/account/passkey/register/finish", `{"name":"k","attestation":{"a":1}}`},
		{"GET", "/api/v1/account/passkey/credentials", ""},
		{"DELETE", "/api/v1/account/passkey/credentials/x", ""},
	} {
		if w := do(ih, tc.method, tc.path, tc.body, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s %s on internal face: code = %d, want 404", tc.method, tc.path, w.Code)
		}
	}
}
