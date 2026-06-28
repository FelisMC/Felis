package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Username-collision reclaim (spec §B3) is Mojang-priority: when a non-genuine
// player squats a name the official service also issues, velocity bars the
// squatter's UUID and stashes its data for 30 days. These tests pin the
// Go-verifiable data layer of that flow — the bar, the stash, idempotency, and
// the one safety property that must never regress: the block is keyed by UUID, so
// the genuine Mojang player (same name, DIFFERENT UUID) is never caught.

// TestReclaimBarsSquatterAndStashes is the happy path: a reclaim bars the
// squatter UUID, the subsequent gate check for that UUID reports barred, and the
// data hold lands with the API-clock 30-day expiry.
func TestReclaimBarsSquatterAndStashes(t *testing.T) {
	const squatter = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	ih := api.InternalHandler()

	body := `{"squatter_uuid":"` + squatter + `","username":"Notch","data_ref":"s3://holds/notch"}`
	w := do(ih, "POST", "/api/v1/internal/player/reclaim", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reclaim: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	b := acctBody(t, w)
	if b["blacklisted"] != true || b["username"] != "Notch" {
		t.Fatalf("reclaim body = %v, want blacklisted:true username:Notch", b)
	}

	// The squatter UUID is now barred — what the velocity login gate checks.
	if !repo.blacklist[squatter] {
		t.Fatal("squatter UUID was not barred")
	}
	// The data is stashed for inherit with the authoritative-clock 30-day window.
	hold, ok := repo.holds[squatter]
	if !ok {
		t.Fatal("no data hold was stashed for the squatter")
	}
	if hold.username != "Notch" || hold.dataRef != "s3://holds/notch" {
		t.Errorf("hold = %+v, want username:Notch dataRef:s3://holds/notch", hold)
	}
	if want := api.now().Add(reclaimHoldTTL); !hold.expiresAt.Equal(want) {
		t.Errorf("hold.expiresAt = %v, want %v", hold.expiresAt, want)
	}

	// The gate check for the barred UUID reports it.
	w = do(ih, "GET", "/api/v1/internal/player/blacklist/"+squatter, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("blacklist check: code = %d (%s)", w.Code, w.Body.String())
	}
	if acctBody(t, w)["blacklisted"] != true {
		t.Fatalf("blacklist check body = %s, want blacklisted:true", w.Body.String())
	}
}

// TestReclaimNeverCatchesGenuineMojangPlayer is the load-bearing safety property:
// the block is keyed by UUID, never by the contested name. A genuine Mojang
// player shares the username but carries a different UUID, so the gate must let
// them through even after the squatter is barred. If this ever regresses,正版优先
// becomes正版连不上 — the exact outcome the design forbids.
func TestReclaimNeverCatchesGenuineMojangPlayer(t *testing.T) {
	const (
		squatter = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		genuine  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" // same name, real owner
	)
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	ih := api.InternalHandler()

	body := `{"squatter_uuid":"` + squatter + `","username":"Notch"}`
	if w := do(ih, "POST", "/api/v1/internal/player/reclaim", body, nil); w.Code != http.StatusOK {
		t.Fatalf("reclaim: code = %d (%s)", w.Code, w.Body.String())
	}

	// The genuine owner — identical username, different UUID — is NOT barred.
	w := do(ih, "GET", "/api/v1/internal/player/blacklist/"+genuine, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("genuine check: code = %d (%s)", w.Code, w.Body.String())
	}
	if got := acctBody(t, w)["blacklisted"]; got != false {
		t.Fatalf("genuine Mojang player blacklisted = %v, want false", got)
	}
}

// TestReclaimProtectsAdminOnYggdrasil is the admin-on-Yggdrasil exception (spec §B3),
// the second safety property alongside the genuine-Mojang case: a Linked
// Operator/SysAdmin who authenticates through the third-party Yggdrasil is staff on the
// Login Server, not a Mojang squatter, so a Mojang-priority reclaim must REFUSE rather
// than bar them. The reclaim is declined (409 protected_admin), nothing is barred or
// stashed, the login gate consequently passes the admin's UUID end-to-end, and the
// refusal lands in the audit log under a DISTINCT action so it can never be mistaken
// for a bar.
func TestReclaimProtectsAdminOnYggdrasil(t *testing.T) {
	const adminUUID = "0a11dead-0000-0000-0000-00000000ad11"
	repo := newFakeRepo()
	// An Operator who linked in-game through the third-party Yggdrasil (auth_source).
	repo.staff["operator1"] = &StaffUser{ID: "op-1", Username: "operator1", Role: "admin", PasswordHash: "$2a$10$VnJ5kZqZ9bQmsCp1uoQ3qO"}
	repo.links[adminUUID] = "op-1"
	repo.linkAuthSource[adminUUID] = authSourceThirdParty

	api := newTestAPI(repo, newFakeCluster())
	ih := api.InternalHandler()

	body := `{"squatter_uuid":"` + adminUUID + `","username":"Operator"}`
	w := do(ih, "POST", "/api/v1/internal/player/reclaim", body, nil)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "protected_admin" {
		t.Fatalf("reclaim of a protected admin: code = %d body %s, want 409 protected_admin", w.Code, w.Body.String())
	}

	// Nothing was barred and nothing was stashed — the reclaim was refused outright.
	if len(repo.blacklist) != 0 || len(repo.holds) != 0 {
		t.Fatalf("a refused reclaim must not bar or stash anything: blacklist=%v holds=%v", repo.blacklist, repo.holds)
	}
	// End-to-end: the login gate consequently passes the admin's UUID.
	w = do(ih, "GET", "/api/v1/internal/player/blacklist/"+adminUUID, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("gate check: code = %d (%s)", w.Code, w.Body.String())
	}
	if got := acctBody(t, w)["blacklisted"]; got != false {
		t.Fatalf("protected admin blacklisted = %v, want false", got)
	}
	// The refusal is audited under a distinct action, separable from a real bar.
	if len(repo.audits) != 1 {
		t.Fatalf("audits = %d, want 1 refusal row", len(repo.audits))
	}
	a := repo.audits[0]
	if a.Action != "player.reclaim.refused" || a.Actor != "velocity" || a.Source != "internal" {
		t.Fatalf("audit = %+v, want player.reclaim.refused/velocity/internal", a)
	}
	var p map[string]string
	if err := json.Unmarshal(a.Payload, &p); err != nil {
		t.Fatalf("audit payload not JSON: %v (%s)", err, a.Payload)
	}
	if p["reason"] != "protected_admin" || p["squatter_uuid"] != adminUUID {
		t.Errorf("audit payload = %v, want reason:protected_admin squatter_uuid:%s", p, adminUUID)
	}
}

// TestReclaimAdminProtectionScope pins the exact predicate the exception turns on so a
// future broadening or narrowing of it cannot pass silently. Protection holds for, and
// ONLY for, a linked holder that is BOTH authenticated via the third-party Yggdrasil
// AND an admin:
//   - a thirdparty NON-admin player is still reclaimed (pins role='admin') — Mojang
//     priority must keep displacing ordinary squatters;
//   - a Mojang-authenticated admin is still reclaimed (pins auth_source='thirdparty') —
//     an admin's Mojang identity has no Login-Server name to protect (and Mojang names
//     are unique, so this is operationally moot, but it locks the conjunct);
//   - an SSO Operator with NO local password is still protected (pins the deliberate
//     ABSENCE of a password_hash test) — signing in via Cloudflare Access (§14) leaves
//     role='admin' with a NULL hash, and that holder must be protected all the same.
func TestReclaimAdminProtectionScope(t *testing.T) {
	const squatter = "0a11dead-0000-0000-0000-00000000ad11"
	cases := []struct {
		name      string
		role      string
		auth      string
		passHash  string
		protected bool // true: reclaim refused (409); false: reclaim succeeds (200, barred)
	}{
		{"thirdparty non-admin is reclaimed", "user", authSourceThirdParty, "", false},
		{"mojang admin is reclaimed", "admin", authSourceMojang, "$2a$10$VnJ5kZqZ9bQmsCp1uoQ3qO", false},
		{"sso admin without local password is protected", "admin", authSourceThirdParty, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			repo.staff["holder"] = &StaffUser{ID: "h-1", Username: "holder", Role: tc.role, PasswordHash: tc.passHash}
			repo.links[squatter] = "h-1"
			repo.linkAuthSource[squatter] = tc.auth
			api := newTestAPI(repo, newFakeCluster())
			ih := api.InternalHandler()

			body := `{"squatter_uuid":"` + squatter + `","username":"Holder"}`
			w := do(ih, "POST", "/api/v1/internal/player/reclaim", body, nil)

			if tc.protected {
				if w.Code != http.StatusConflict || decodeErr(t, w) != "protected_admin" {
					t.Fatalf("code = %d body %s, want 409 protected_admin", w.Code, w.Body.String())
				}
				if len(repo.blacklist) != 0 || len(repo.holds) != 0 {
					t.Fatal("a protected holder must not be barred or stashed")
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d body %s, want 200 (reclaim should proceed)", w.Code, w.Body.String())
			}
			if !repo.blacklist[squatter] {
				t.Fatal("an unprotected squatter must be barred — Mojang priority still holds")
			}
		})
	}
}

// TestReclaimIsIdempotent proves a retried velocity callback is harmless: a repeat
// reclaim of an already-barred UUID still answers 200 and does not disturb the
// original hold (matching the ON CONFLICT DO NOTHING in both inserts).
func TestReclaimIsIdempotent(t *testing.T) {
	const squatter = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	ih := api.InternalHandler()

	first := `{"squatter_uuid":"` + squatter + `","username":"Herobrine","data_ref":"ref-1"}`
	if w := do(ih, "POST", "/api/v1/internal/player/reclaim", first, nil); w.Code != http.StatusOK {
		t.Fatalf("first reclaim: code = %d (%s)", w.Code, w.Body.String())
	}
	original := repo.holds[squatter]

	// A retry with a different data_ref must not overwrite the original stash.
	retry := `{"squatter_uuid":"` + squatter + `","username":"Herobrine","data_ref":"ref-2"}`
	if w := do(ih, "POST", "/api/v1/internal/player/reclaim", retry, nil); w.Code != http.StatusOK {
		t.Fatalf("retry reclaim: code = %d (%s)", w.Code, w.Body.String())
	}
	if got := repo.holds[squatter]; got != original {
		t.Errorf("idempotent retry altered the hold: got %+v, want %+v", got, original)
	}
}

// TestReclaimRetryReportsOriginalWindow is the regression for the response-honesty
// fix: the squatter reconnects days later, velocity re-fires the reclaim, and the
// hold keeps its FIRST 30-day window (ON CONFLICT preserves it). The response must
// report that persisted window, NOT a fresh now()+30d — otherwise velocity tells
// the player a kept-until date the stored hold does not actually have. A frozen
// clock cannot catch this, so the clock is advanced between the two reclaims.
func TestReclaimRetryReportsOriginalWindow(t *testing.T) {
	const squatter = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	ih := api.InternalHandler()

	body := `{"squatter_uuid":"` + squatter + `","username":"Notch"}`
	w := do(ih, "POST", "/api/v1/internal/player/reclaim", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("first reclaim: code = %d (%s)", w.Code, w.Body.String())
	}
	first := acctBody(t, w)["hold_expires_at"]

	// Three days pass; velocity re-fires the reclaim on the squatter's next attempt.
	clock = clock.Add(72 * time.Hour)
	w = do(ih, "POST", "/api/v1/internal/player/reclaim", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("retry reclaim: code = %d (%s)", w.Code, w.Body.String())
	}
	second := acctBody(t, w)["hold_expires_at"]

	if first != second {
		t.Fatalf("retry hold_expires_at = %v, want the original %v — the response must reflect the persisted hold, not a fresh clock", second, first)
	}
	// And it must equal the first reclaim's window, not the retry-time clock.
	if want := time.Unix(1_700_000_000, 0).Add(reclaimHoldTTL).UTC().Format(time.RFC3339Nano); second != want {
		t.Errorf("hold_expires_at = %v, want %v (first reclaim's window)", second, want)
	}
}

// TestReclaimAudited proves a reclaim writes a security-significant accountability
// row: a player is barred and their world stashed, so the event lands in
// audit_logs as player.reclaim from velocity over the internal source, with the
// contested name and squatter UUID in the structured payload.
func TestReclaimAudited(t *testing.T) {
	const squatter = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	ih := api.InternalHandler()

	body := `{"squatter_uuid":"` + squatter + `","username":"Steve"}`
	if w := do(ih, "POST", "/api/v1/internal/player/reclaim", body, nil); w.Code != http.StatusOK {
		t.Fatalf("reclaim: code = %d (%s)", w.Code, w.Body.String())
	}
	if len(repo.audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(repo.audits))
	}
	a := repo.audits[0]
	if a.Action != "player.reclaim" || a.Actor != "velocity" || a.Source != "internal" {
		t.Fatalf("audit = %+v, want player.reclaim/velocity/internal", a)
	}
	var p map[string]string
	if err := json.Unmarshal(a.Payload, &p); err != nil {
		t.Fatalf("audit payload not JSON: %v (%s)", err, a.Payload)
	}
	if p["username"] != "Steve" || p["squatter_uuid"] != squatter {
		t.Errorf("audit payload = %v, want username:Steve squatter_uuid:%s", p, squatter)
	}
}

// TestReclaimValidation is the input matrix: a reclaim with no UUID or no username
// is a 400, strict decoding rejects unknown fields, and a rejected request neither
// bars anyone nor stashes anything.
func TestReclaimValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing squatter_uuid", `{"username":"Notch"}`},
		{"empty squatter_uuid", `{"squatter_uuid":"","username":"Notch"}`},
		{"missing username", `{"squatter_uuid":"aaaa"}`},
		{"empty username", `{"squatter_uuid":"aaaa","username":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			api := newTestAPI(repo, newFakeCluster())
			w := do(api.InternalHandler(), "POST", "/api/v1/internal/player/reclaim", tc.body, nil)
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
				t.Fatalf("code = %d body %s, want 400 bad_request", w.Code, w.Body.String())
			}
			if len(repo.blacklist) != 0 || len(repo.holds) != 0 {
				t.Fatal("a rejected reclaim must not bar or stash anything")
			}
		})
	}

	t.Run("unknown field rejected", func(t *testing.T) {
		repo := newFakeRepo()
		api := newTestAPI(repo, newFakeCluster())
		body := `{"squatter_uuid":"aaaa","username":"Notch","reason":"smuggled"}`
		w := do(api.InternalHandler(), "POST", "/api/v1/internal/player/reclaim", body, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("strict decode must reject unknown field, code = %d (%s)", w.Code, w.Body.String())
		}
	})
}

// TestReclaimFaceSeparation enforces that the reclaim and gate-check endpoints are
// internal-only: velocity holds a service token, a web Principal never reaches
// them. Crossing onto the external face must 404, not silently work — a logged-in
// user could otherwise bar an arbitrary UUID.
func TestReclaimFaceSeparation(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: user}
	eh := api.ExternalHandler()

	body := `{"squatter_uuid":"aaaa","username":"Notch"}`
	if w := do(eh, "POST", "/api/v1/internal/player/reclaim", body, nil); w.Code != http.StatusNotFound {
		t.Errorf("reclaim on external face: code = %d, want 404", w.Code)
	}
	if w := do(eh, "GET", "/api/v1/internal/player/blacklist/aaaa", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("blacklist check on external face: code = %d, want 404", w.Code)
	}
}
