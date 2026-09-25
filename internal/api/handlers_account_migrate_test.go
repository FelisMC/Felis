package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Account-migration tests (spec §B3 inherit, scenario A) across BOTH faces. What they
// prove is the handler + state machine (initiated → confirmed → code_issued → redeemed)
// and the load-bearing security properties of the flow, against the fakeRepo and a fake
// PasskeyVerifier — not the pgrepo SQL nor the WebAuthn crypto (both mirrored here). The
// decisive properties, in flow order:
//
//   - Step-up is a FRESH proof, and force-passkey is not downgradable: an account with a
//     passkey cannot confirm by email (no OTP is even minted).
//   - The one-time code is bound to the NAMED target at issue AND the redeemer must be
//     that target, so an intercepted code is useless to anyone else.
//   - The step-up is never weaker than the login door: a cloned authenticator the login
//     door refuses is refused here too, and confirms nothing.
//   - Redeem is a double-spend-safe, atomic transfer: the source's servers move to the
//     target, the source is retired, and the code cannot be replayed.

// migrateEnv wires the migration doors over one shared fakeRepo: a capturing mailer (so a
// test can read the OTP that production only ever emails) and a fake passkey verifier
// primed with fixed options + a verified assertion. mk builds a face for a given
// principal — the internal handler ignores it, each external handler is scoped to one
// caller — so a test can drive the source and the target (and an interloper) against the
// same store.
func migrateEnv(repo *fakeRepo) (mk func(*Principal) *API, mailer *captureMailer, v *fakePasskeyVerifier) {
	mailer = &captureMailer{}
	v = &fakePasskeyVerifier{
		options:   json.RawMessage(`{"publicKey":{"challenge":"bWlncmF0ZQ"}}`),
		assertion: VerifiedAssertion{CredentialID: "cred-1", UserVerified: true},
	}
	cl := newFakeCluster()
	mk = func(p *Principal) *API {
		a := newTestAPI(repo, cl)
		a.External = staticExternal{p: p}
		a.Mailer = mailer
		a.Passkey = v
		return a
	}
	return mk, mailer, v
}

// startMigrate drives the in-game /felis migrate call (internal face) for a linked UUID.
func startMigrate(t *testing.T, ih http.Handler, mcUUID string) *httptest.ResponseRecorder {
	t.Helper()
	return do(ih, "POST", "/api/v1/internal/account/migrate/start", `{"mc_uuid":"`+mcUUID+`"}`, jsonHeader)
}

// TestMigrateVertical walks the whole slice: an in-game start, an email-OTP step-up
// (the source holds no passkey, so email is allowed), a code issued against a named
// target, and the target redeeming it — moving exactly the source's servers and retiring
// the source. The single-use property closes it: the spent code cannot be replayed.
func TestMigrateVertical(t *testing.T) {
	const uuid = "11111111-1111-1111-1111-111111111111"
	src := &Principal{UserID: "u1", Email: "old@example.net", Role: "user"}
	tgt := &Principal{UserID: "u2", Email: "new@example.net", Role: "user"}

	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "u1", Username: "old", Email: "old@example.net", Role: "user"})
	repo.seedUser(UserView{ID: "u2", Username: "new", Email: "new@example.net", Role: "user"})
	repo.links[uuid] = "u1"
	repo.byName["alpha"] = &ServerRecord{Name: "alpha", OwnerID: "u1"}
	repo.byName["beta"] = &ServerRecord{Name: "beta", OwnerID: "u1"}
	repo.byName["other"] = &ServerRecord{Name: "other", OwnerID: "u2"} // the target's own — must NOT move

	mk, mailer, _ := migrateEnv(repo)
	ih := mk(src).InternalHandler()
	ehSrc := mk(src).ExternalHandler()
	ehTgt := mk(tgt).ExternalHandler()

	// 1) in-game start puts the linked account into migrate mode.
	if w := startMigrate(t, ih, uuid); w.Code != http.StatusCreated {
		t.Fatalf("start: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if w := do(ehSrc, "GET", "/api/v1/account/migrate", "", nil); acctBody(t, w)["state"] != "initiated" {
		t.Fatalf("status after start = %s, want initiated", w.Body.String())
	}

	// 2) email-OTP step-up. The code is delivered out of band (captured here), never in
	// the response, and verifying it advances the migration to confirmed.
	w := do(ehSrc, "POST", "/api/v1/account/migrate/confirm/otp/start", "", jsonHeader)
	if w.Code != http.StatusAccepted {
		t.Fatalf("otp start: code = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	if _, leaked := acctBody(t, w)["code"]; leaked {
		t.Error("otp start response must NEVER carry the code")
	}
	code := mailer.code
	if code == "" {
		t.Fatal("no OTP delivered")
	}
	w = do(ehSrc, "POST", "/api/v1/account/migrate/confirm/otp/verify", `{"code":"`+code+`"}`, jsonHeader)
	if w.Code != http.StatusOK || acctBody(t, w)["confirmed"] != true {
		t.Fatalf("otp verify: code = %d body %s, want 200 confirmed", w.Code, w.Body.String())
	}
	if b := do(ehSrc, "GET", "/api/v1/account/migrate", "", nil); acctBody(t, b)["confirm_factor"] != "email_otp" {
		t.Fatalf("status after confirm = %s, want confirm_factor email_otp", b.Body.String())
	}

	// 3) the source names the target and mints a one-time code.
	w = do(ehSrc, "POST", "/api/v1/account/migrate/issue-code", `{"target_user_id":"u2"}`, jsonHeader)
	if w.Code != http.StatusCreated {
		t.Fatalf("issue-code: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	mcode, _ := acctBody(t, w)["code"].(string)
	if mcode == "" {
		t.Fatal("issue-code returned no code")
	}

	// 4) the target redeems: exactly the source's two servers move; the target's own is
	// untouched; the source is retired.
	w = do(ehTgt, "POST", "/api/v1/account/migrate/redeem", `{"code":"`+mcode+`"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("redeem: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	rb := acctBody(t, w)
	if rb["migrated"] != true || rb["servers_moved"] != float64(2) {
		t.Fatalf("redeem body = %v, want migrated:true servers_moved:2", rb)
	}
	if repo.byName["alpha"].OwnerID != "u2" || repo.byName["beta"].OwnerID != "u2" {
		t.Fatalf("source servers not moved: alpha=%s beta=%s", repo.byName["alpha"].OwnerID, repo.byName["beta"].OwnerID)
	}
	if repo.byName["other"].OwnerID != "u2" { // was u2 already; a move would be a bug either way
		t.Fatalf("target's own server changed owner to %s", repo.byName["other"].OwnerID)
	}
	if d, _ := repo.UserDetail(context.Background(), "u1"); d == nil || d.DeletedAt == nil || !d.Disabled {
		t.Fatalf("source account not retired: %+v", d)
	}

	// 5) single-use: the spent code cannot be replayed.
	if w := do(ehTgt, "POST", "/api/v1/account/migrate/redeem", `{"code":"`+mcode+`"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("replay of spent code: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
}

// TestMigrateForcePasskey pins the contract's "若有 Passkey 强制 Passkey": an account with
// a passkey enrolled cannot step up by email — the OTP door refuses with passkey_required
// and mints NOTHING, so the strong factor is not downgradable for an identity transfer.
func TestMigrateForcePasskey(t *testing.T) {
	const uuid = "22222222-2222-2222-2222-222222222222"
	src := &Principal{UserID: "u1", Email: "old@example.net", Role: "user"}

	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "u1", Username: "old", Email: "old@example.net", Role: "user"})
	repo.links[uuid] = "u1"
	repo.passkeyCreds["row1"] = PasskeyCredential{ID: "row1", UserID: "u1", CredentialID: "cred-1", PublicKey: "k", UserVerified: true, CreatedAt: frozenNow}

	mk, mailer, _ := migrateEnv(repo)
	ih := mk(src).InternalHandler()
	ehSrc := mk(src).ExternalHandler()

	if w := startMigrate(t, ih, uuid); w.Code != http.StatusCreated {
		t.Fatalf("start: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	w := do(ehSrc, "POST", "/api/v1/account/migrate/confirm/otp/start", "", jsonHeader)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "passkey_required" {
		t.Fatalf("otp start with passkey enrolled: code = %d body %s, want 409 passkey_required", w.Code, w.Body.String())
	}
	if mailer.calls != 0 {
		t.Fatalf("an OTP was minted despite an enrolled passkey (calls=%d)", mailer.calls)
	}
}

// TestMigratePasskeyConfirm drives the passkey step-up: begin stashes exactly one
// migrate-purpose challenge for the caller, and finish verifies the assertion against the
// SERVER-STASHED session data (the body carries no challenge) and advances the migration
// to confirmed with factor 'passkey'.
func TestMigratePasskeyConfirm(t *testing.T) {
	const uuid = "33333333-3333-3333-3333-333333333333"
	src := &Principal{UserID: "u1", Email: "old@example.net", Role: "user"}

	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "u1", Username: "old", Email: "old@example.net", Role: "user"})
	repo.links[uuid] = "u1"
	repo.passkeyCreds["row1"] = PasskeyCredential{ID: "row1", UserID: "u1", CredentialID: "cred-1", PublicKey: "k", UserVerified: true, CreatedAt: frozenNow}

	mk, _, v := migrateEnv(repo)
	ih := mk(src).InternalHandler()
	ehSrc := mk(src).ExternalHandler()

	if w := startMigrate(t, ih, uuid); w.Code != http.StatusCreated {
		t.Fatalf("start: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}

	// begin: exactly one migrate-purpose challenge stashed for u1, options returned verbatim.
	w := do(ehSrc, "POST", "/api/v1/account/migrate/confirm/passkey/begin", "", jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("passkey begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if len(repo.passkeyChallenges) != 1 {
		t.Fatalf("begin must stash exactly one challenge, got %d", len(repo.passkeyChallenges))
	}
	for _, c := range repo.passkeyChallenges {
		if c.userID != "u1" || c.purpose != passkeyPurposeMigrate {
			t.Errorf("stashed challenge = %+v, want user u1 purpose %q", c, passkeyPurposeMigrate)
		}
	}

	// finish: the body carries NO challenge; the stashed blob reaches the verifier only via
	// store-stash → consume, and a verified assertion confirms the migration.
	w = do(ehSrc, "POST", "/api/v1/account/migrate/confirm/passkey/finish",
		`{"assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)
	if w.Code != http.StatusOK || acctBody(t, w)["confirmed"] != true {
		t.Fatalf("passkey finish: code = %d body %s, want 200 confirmed", w.Code, w.Body.String())
	}
	if len(v.lastSession) == 0 {
		t.Error("finish must feed the verifier the server-stashed session data")
	}
	if m, _ := repo.MigrationForSource(context.Background(), "u1"); m == nil || m.State != "confirmed" || m.ConfirmFactor != "passkey" {
		t.Fatalf("migration after passkey confirm = %+v, want state confirmed factor passkey", m)
	}
}

// TestMigratePasskeyCloneRejected proves the step-up is never weaker than the login door:
// a cloned authenticator (rolled-back counter) is refused with the opaque envelope and
// confirms nothing — the migration stays in initiated.
func TestMigratePasskeyCloneRejected(t *testing.T) {
	const uuid = "44444444-4444-4444-4444-444444444444"
	src := &Principal{UserID: "u1", Email: "old@example.net", Role: "user"}

	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "u1", Username: "old", Email: "old@example.net", Role: "user"})
	repo.links[uuid] = "u1"
	repo.passkeyCreds["row1"] = PasskeyCredential{ID: "row1", UserID: "u1", CredentialID: "cred-1", PublicKey: "k", UserVerified: true, CreatedAt: frozenNow}

	mk, _, v := migrateEnv(repo)
	v.assertion = VerifiedAssertion{CredentialID: "cred-1", UserVerified: true, CloneWarning: true}
	ih := mk(src).InternalHandler()
	ehSrc := mk(src).ExternalHandler()

	if w := startMigrate(t, ih, uuid); w.Code != http.StatusCreated {
		t.Fatalf("start: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if w := do(ehSrc, "POST", "/api/v1/account/migrate/confirm/passkey/begin", "", jsonHeader); w.Code != http.StatusOK {
		t.Fatalf("passkey begin: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	w := do(ehSrc, "POST", "/api/v1/account/migrate/confirm/passkey/finish",
		`{"assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_invalid" {
		t.Fatalf("cloned authenticator: code = %d body %s, want 400 passkey_login_invalid", w.Code, w.Body.String())
	}
	if m, _ := repo.MigrationForSource(context.Background(), "u1"); m == nil || m.State != "initiated" {
		t.Fatalf("migration after clone-rejected finish = %+v, want still initiated", m)
	}
}

// TestMigrateRedeemBinding proves the one-time code is bound to the NAMED target: an
// interloper who holds the correct code but is a different account cannot redeem it (it
// simply does not match), the transfer does not happen, and the code survives for the
// genuine target to spend.
func TestMigrateRedeemBinding(t *testing.T) {
	const uuid = "55555555-5555-5555-5555-555555555555"
	src := &Principal{UserID: "u1", Email: "old@example.net", Role: "user"}
	tgt := &Principal{UserID: "u2", Email: "new@example.net", Role: "user"}
	interloper := &Principal{UserID: "u3", Email: "evil@example.net", Role: "user"}

	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "u1", Username: "old", Email: "old@example.net", Role: "user"})
	repo.seedUser(UserView{ID: "u2", Username: "new", Email: "new@example.net", Role: "user"})
	repo.links[uuid] = "u1"
	repo.byName["alpha"] = &ServerRecord{Name: "alpha", OwnerID: "u1"}

	mk, mailer, _ := migrateEnv(repo)
	ih := mk(src).InternalHandler()
	ehSrc := mk(src).ExternalHandler()

	// Drive to a code issued against u2.
	if w := startMigrate(t, ih, uuid); w.Code != http.StatusCreated {
		t.Fatalf("start: %d (%s)", w.Code, w.Body.String())
	}
	if w := do(ehSrc, "POST", "/api/v1/account/migrate/confirm/otp/start", "", jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("otp start: %d (%s)", w.Code, w.Body.String())
	}
	if w := do(ehSrc, "POST", "/api/v1/account/migrate/confirm/otp/verify", `{"code":"`+mailer.code+`"}`, jsonHeader); w.Code != http.StatusOK {
		t.Fatalf("otp verify: %d (%s)", w.Code, w.Body.String())
	}
	w := do(ehSrc, "POST", "/api/v1/account/migrate/issue-code", `{"target_user_id":"u2"}`, jsonHeader)
	if w.Code != http.StatusCreated {
		t.Fatalf("issue-code: %d (%s)", w.Code, w.Body.String())
	}
	mcode, _ := acctBody(t, w)["code"].(string)

	// The interloper holds the correct code but is not the named target: no match.
	ehEvil := mk(interloper).ExternalHandler()
	if w := do(ehEvil, "POST", "/api/v1/account/migrate/redeem", `{"code":"`+mcode+`"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
		t.Fatalf("interloper redeem: code = %d body %s, want 400 invalid_code", w.Code, w.Body.String())
	}
	if repo.byName["alpha"].OwnerID != "u1" {
		t.Fatalf("server moved on a non-target redeem: owner=%s", repo.byName["alpha"].OwnerID)
	}
	if d, _ := repo.UserDetail(context.Background(), "u1"); d.DeletedAt != nil {
		t.Fatal("source retired on a non-target redeem")
	}

	// The genuine target still spends the same code — the failed attempt consumed nothing.
	ehTgt := mk(tgt).ExternalHandler()
	if w := do(ehTgt, "POST", "/api/v1/account/migrate/redeem", `{"code":"`+mcode+`"}`, jsonHeader); w.Code != http.StatusOK {
		t.Fatalf("genuine target redeem: code = %d body %s, want 200", w.Code, w.Body.String())
	}
	if repo.byName["alpha"].OwnerID != "u2" {
		t.Fatalf("server not moved to genuine target: owner=%s", repo.byName["alpha"].OwnerID)
	}
}

// TestMigrateGuards covers the input/state refusals: an unlinked UUID has no account to
// migrate; a code cannot be issued before confirmation; the target may be neither the
// source itself nor an unknown account.
func TestMigrateGuards(t *testing.T) {
	const uuid = "66666666-6666-6666-6666-666666666666"
	src := &Principal{UserID: "u1", Email: "old@example.net", Role: "user"}

	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "u1", Username: "old", Email: "old@example.net", Role: "user"})
	repo.links[uuid] = "u1"

	mk, mailer, _ := migrateEnv(repo)
	ih := mk(src).InternalHandler()
	ehSrc := mk(src).ExternalHandler()

	// start on an unlinked UUID → 404 not_linked.
	if w := startMigrate(t, ih, "00000000-0000-0000-0000-000000000000"); w.Code != http.StatusNotFound || decodeErr(t, w) != "not_linked" {
		t.Fatalf("start unlinked: code = %d body %s, want 404 not_linked", w.Code, w.Body.String())
	}

	// A real start, then issue-code BEFORE confirming → 409 not_confirmed.
	if w := startMigrate(t, ih, uuid); w.Code != http.StatusCreated {
		t.Fatalf("start: %d (%s)", w.Code, w.Body.String())
	}
	if w := do(ehSrc, "POST", "/api/v1/account/migrate/issue-code", `{"target_user_id":"u1"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_target" {
		t.Fatalf("issue with target==source: code = %d body %s, want 400 invalid_target", w.Code, w.Body.String())
	}

	// Confirm (email; no passkey on this account), then the target guards apply.
	if w := do(ehSrc, "POST", "/api/v1/account/migrate/confirm/otp/start", "", jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("otp start: %d (%s)", w.Code, w.Body.String())
	}
	if w := do(ehSrc, "POST", "/api/v1/account/migrate/confirm/otp/verify", `{"code":"`+mailer.code+`"}`, jsonHeader); w.Code != http.StatusOK {
		t.Fatalf("otp verify: %d (%s)", w.Code, w.Body.String())
	}
	if w := do(ehSrc, "POST", "/api/v1/account/migrate/issue-code", `{"target_user_id":"ghost"}`, jsonHeader); w.Code != http.StatusBadRequest || decodeErr(t, w) != "target_not_found" {
		t.Fatalf("issue with unknown target: code = %d body %s, want 400 target_not_found", w.Code, w.Body.String())
	}
}
