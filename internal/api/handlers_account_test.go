package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// acctBody decodes a success body into a generic object for field assertions.
func acctBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
	}
	return m
}

// TestAccountLinkVertical walks the whole §10 flow across both faces and proves
// the point of the slice: linking reconnects the claim vertical that handleClaim
// (handlers_user.go:100) otherwise dead-ends with 412 not_linked.
func TestAccountLinkVertical(t *testing.T) {
	const mcUUID = "11111111-1111-1111-1111-111111111111"
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}

	repo := newFakeRepo()
	// Pre-arm the downstream claim gates so a *successful* claim becomes possible
	// the instant the link gate clears — that is what demonstrates the unblock.
	repo.quota["u1"] = true
	repo.claimOK["survival"] = true

	api := newTestAPI(repo, claimCluster())
	api.External = staticExternal{p: user}
	ih := api.InternalHandler()
	eh := api.ExternalHandler()

	// Before linking, the claim dead-ends at the link gate.
	if w := do(eh, "POST", "/api/v1/servers/survival/claim", "", nil); w.Code != http.StatusPreconditionFailed || decodeErr(t, w) != "not_linked" {
		t.Fatalf("pre-link claim: code = %d body %s, want 412 not_linked", w.Code, w.Body.String())
	}

	// 1) in-game /link mints a one-time code (internal face).
	w := do(ih, "POST", "/api/v1/internal/account/link/code", `{"mc_uuid":"`+mcUUID+`"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint code: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	code, _ := acctBody(t, w)["code"].(string)
	if len(code) != linkCodeLen {
		t.Fatalf("minted code %q: len = %d, want %d", code, len(code), linkCodeLen)
	}

	// 2) the player submits the code on the panel (external face).
	w = do(eh, "POST", "/api/v1/account/link/verify", `{"code":"`+code+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if b := acctBody(t, w); b["linked"] != true || b["mc_uuid"] != mcUUID || b["auth_source"] != authSourceMojang {
		t.Fatalf("verify body = %v, want linked:true mc_uuid:%s auth_source:%s", b, mcUUID, authSourceMojang)
	}
	// The link is audited as account.link by the principal's Access email.
	if n := len(repo.audits); n != 1 || repo.audits[0].Action != "account.link" || repo.audits[0].Actor != "u1@example.net" {
		t.Fatalf("link audit not written as expected: %+v", repo.audits)
	}

	// 3) the identical claim now passes the link gate and succeeds — the vertical
	// is reconnected, which is the whole reason this slice exists.
	if w := do(eh, "POST", "/api/v1/servers/survival/claim", "", nil); w.Code != http.StatusOK {
		t.Fatalf("post-link claim: code = %d body %s, want 200", w.Code, w.Body.String())
	}
}

// TestCreateLinkCode covers the internal mint endpoint: input validation, strict
// decoding, and that a real code lands in the store with the API-clock TTL.
func TestCreateLinkCode(t *testing.T) {
	const mcUUID = "22222222-2222-2222-2222-222222222222"
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	ih := api.InternalHandler()

	t.Run("missing uuid -> 400", func(t *testing.T) {
		w := do(ih, "POST", "/api/v1/internal/account/link/code", `{}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("unknown field rejected (no user_id smuggling)", func(t *testing.T) {
		// The schema deliberately has no user_id on a code; a caller must not be able
		// to introduce one via an extra field.
		w := do(ih, "POST", "/api/v1/internal/account/link/code", `{"mc_uuid":"`+mcUUID+`","user_id":"u1"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("strict decode must reject unknown field, code = %d (%s)", w.Code, w.Body.String())
		}
	})
	t.Run("mints into store with TTL", func(t *testing.T) {
		w := do(ih, "POST", "/api/v1/internal/account/link/code", `{"mc_uuid":"`+mcUUID+`"}`, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
		}
		code, _ := acctBody(t, w)["code"].(string)
		rec, ok := repo.linkCodes[code]
		if !ok {
			t.Fatalf("minted code %q was not persisted", code)
		}
		if rec.mcUUID != mcUUID {
			t.Errorf("stored mc_uuid = %q, want %q", rec.mcUUID, mcUUID)
		}
		// An omitted auth_source defaults to the Mojang-priority source.
		if rec.authSource != authSourceMojang {
			t.Errorf("default authSource = %q, want %q", rec.authSource, authSourceMojang)
		}
		if want := api.now().Add(linkCodeTTL); !rec.expiresAt.Equal(want) {
			t.Errorf("expiresAt = %v, want %v", rec.expiresAt, want)
		}
		for _, c := range code {
			if !strings.ContainsRune(linkCodeAlphabet, c) {
				t.Errorf("code %q contains out-of-alphabet rune %q", code, c)
			}
		}
	})
	t.Run("panel_url points at the web console", func(t *testing.T) {
		// The mint response carries the redeem address so every plugin renders the
		// same hostname from one source of truth (derived console.<root> here).
		w := do(ih, "POST", "/api/v1/internal/account/link/code", `{"mc_uuid":"`+mcUUID+`"}`, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
		}
		if got := acctBody(t, w)["panel_url"]; got != "https://console."+testRoot {
			t.Errorf("panel_url = %v, want https://console.%s", got, testRoot)
		}
	})
	t.Run("omitted auth_source with a v3 UUID derives thirdparty", func(t *testing.T) {
		// A felis-nano rewrite is a name-based UUIDv3; the version nibble alone must
		// classify it so no Java plugin has to learn the auth_source field.
		const v3UUID = "33333333-3333-3333-8333-333333333333"
		w := do(ih, "POST", "/api/v1/internal/account/link/code", `{"mc_uuid":"`+v3UUID+`"}`, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
		}
		code, _ := acctBody(t, w)["code"].(string)
		if rec := repo.linkCodes[code]; rec.authSource != authSourceThirdParty {
			t.Errorf("derived authSource = %q, want %q", rec.authSource, authSourceThirdParty)
		}
	})
	t.Run("explicit thirdparty is stored", func(t *testing.T) {
		body := `{"mc_uuid":"` + mcUUID + `","auth_source":"` + authSourceThirdParty + `"}`
		w := do(ih, "POST", "/api/v1/internal/account/link/code", body, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
		}
		code, _ := acctBody(t, w)["code"].(string)
		if rec := repo.linkCodes[code]; rec.authSource != authSourceThirdParty {
			t.Errorf("stored authSource = %q, want %q", rec.authSource, authSourceThirdParty)
		}
	})
	t.Run("unrecognised auth_source -> 400", func(t *testing.T) {
		body := `{"mc_uuid":"` + mcUUID + `","auth_source":"litebans"}`
		w := do(ih, "POST", "/api/v1/internal/account/link/code", body, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("code = %d body %s, want 400 bad_request", w.Code, w.Body.String())
		}
	})
}

// TestLinkVerifyRejections is the verify failure matrix. The expired case seeds a
// past-dated code directly: the test clock is frozen, so there is nothing to
// "advance" — planting an already-expired code is the only way to exercise the
// expiry branch.
func TestLinkVerifyRejections(t *testing.T) {
	const mcUUID = "33333333-3333-3333-3333-333333333333"
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	mk := func(repo *fakeRepo) http.Handler {
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		return api.ExternalHandler()
	}

	t.Run("empty code -> 400 bad_request", func(t *testing.T) {
		w := do(mk(newFakeRepo()), "POST", "/api/v1/account/link/verify", `{"code":""}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("whitespace code -> 400 bad_request", func(t *testing.T) {
		w := do(mk(newFakeRepo()), "POST", "/api/v1/account/link/verify", `{"code":"   "}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("unknown field -> 400", func(t *testing.T) {
		w := do(mk(newFakeRepo()), "POST", "/api/v1/account/link/verify", `{"token":"ABCDEFGH"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("strict decode must reject unknown field, code = %d", w.Code)
		}
	})
	t.Run("unknown code -> 400 invalid_code", func(t *testing.T) {
		w := do(mk(newFakeRepo()), "POST", "/api/v1/account/link/verify", `{"code":"NEVERMINT"}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("expired code -> 400 invalid_code", func(t *testing.T) {
		repo := newFakeRepo()
		// One second before the frozen test clock (time.Unix(1_700_000_000, 0)).
		repo.linkCodes["EXPIREDXY"] = fakeLinkCode{mcUUID: mcUUID, expiresAt: time.Unix(1_699_999_999, 0)}
		w := do(mk(repo), "POST", "/api/v1/account/link/verify", `{"code":"EXPIREDXY"}`, nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "invalid_code" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if _, ok := repo.linkCodes["EXPIREDXY"]; !ok {
			t.Error("an expired code must not be consumed")
		}
	})
	t.Run("uuid linked to another user -> 409 already_linked", func(t *testing.T) {
		repo := newFakeRepo()
		repo.links[mcUUID] = "someone-else"
		repo.linkCodes["FRESHCOD"] = fakeLinkCode{mcUUID: mcUUID, authSource: "mojang", expiresAt: time.Unix(1_700_000_600, 0)}
		w := do(mk(repo), "POST", "/api/v1/account/link/verify", `{"code":"FRESHCOD"}`, nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "already_linked" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		// A wrong-user submission must not burn the real owner's pending code.
		if _, ok := repo.linkCodes["FRESHCOD"]; !ok {
			t.Error("a conflicting verify must not consume the code")
		}
	})
}

// TestLinkVerifyIdempotent re-verifies the same (user, uuid) pair with a fresh
// code and expects a clean 200, not a self-conflict.
func TestLinkVerifyIdempotent(t *testing.T) {
	const mcUUID = "44444444-4444-4444-4444-444444444444"
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	repo.links[mcUUID] = "u1" // already linked to THIS user
	repo.linked["u1"] = true
	repo.linkCodes["REVERIFYX"] = fakeLinkCode{mcUUID: mcUUID, authSource: "mojang", expiresAt: time.Unix(1_700_000_600, 0)}

	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: user}
	w := do(api.ExternalHandler(), "POST", "/api/v1/account/link/verify", `{"code":"REVERIFYX"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent re-verify: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := acctBody(t, w)["mc_uuid"]; got != mcUUID {
		t.Errorf("mc_uuid = %v, want %s", got, mcUUID)
	}
	if repo.links[mcUUID] != "u1" {
		t.Errorf("links[%s] = %q, want u1", mcUUID, repo.links[mcUUID])
	}
}

// A link whose account was SOFT-DELETED is unclaimed: a fresh in-game code lets a
// live account take it over (the migrated-source path — retire keeps the link but
// kills the account), while a merely disabled holder keeps its identity so the
// lockout cannot be re-linked away, and neither dead link has in-game standing
// (UserByMCUUID reads it exactly like an unlinked UUID). Audit #33's in-game half.
func TestLinkVerifyTakesOverDeletedLinkOnly(t *testing.T) {
	ctx := context.Background()
	const mcGone = "55555555-5555-5555-5555-555555555555"
	const mcLocked = "66666666-6666-6666-6666-666666666666"
	user := &Principal{UserID: "u-take", Email: "take@example.net", Role: "user"}
	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "u-gone", Username: "gone", Email: "gone@example.net", Role: "user"})
	repo.seedUser(UserView{ID: "u-locked", Username: "locked", Email: "locked@example.net", Role: "user"})
	if err := repo.DeleteUser(ctx, "u-gone", "test"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	repo.links[mcGone] = "u-gone" // a retired source's link outlives the account

	if _, err := repo.UserByMCUUID(ctx, mcGone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByMCUUID(deleted link) = %v, want ErrNotFound (no in-game standing)", err)
	}

	repo.linkCodes["TAKEOVER"] = fakeLinkCode{mcUUID: mcGone, authSource: "mojang", expiresAt: time.Unix(1_700_000_600, 0)}
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: user}
	if w := do(api.ExternalHandler(), "POST", "/api/v1/account/link/verify", `{"code":"TAKEOVER"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("takeover verify: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if repo.links[mcGone] != "u-take" {
		t.Errorf("links[%s] = %q after takeover, want u-take", mcGone, repo.links[mcGone])
	}

	// A disabled (not deleted) holder keeps the identity: 409, code survives, link unmoved.
	if err := repo.SetUserDisabled(ctx, "u-locked", true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	repo.links[mcLocked] = "u-locked"
	repo.linkCodes["LOCKED12"] = fakeLinkCode{mcUUID: mcLocked, expiresAt: time.Unix(1_700_000_600, 0)}
	if _, err := repo.UserByMCUUID(ctx, mcLocked); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByMCUUID(disabled link) = %v, want ErrNotFound", err)
	}
	if w := do(api.ExternalHandler(), "POST", "/api/v1/account/link/verify", `{"code":"LOCKED12"}`, nil); w.Code != http.StatusConflict {
		t.Fatalf("takeover of a disabled holder: code = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if repo.links[mcLocked] != "u-locked" {
		t.Errorf("disabled holder's link moved to %q", repo.links[mcLocked])
	}
	if _, ok := repo.linkCodes["LOCKED12"]; !ok {
		t.Error("refused verify consumed the code")
	}
}

// TestLinkAuthSourcePropagates proves auth_source survives the whole §10 flow: a
// thirdparty source captured in-game at mint reaches the durable link and the
// verify response — the value the web side can never originate itself.
func TestLinkAuthSourcePropagates(t *testing.T) {
	const mcUUID = "55555555-5555-5555-5555-555555555555"
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: user}

	// Mint in-game with the thirdparty Yggdrasil source.
	body := `{"mc_uuid":"` + mcUUID + `","auth_source":"` + authSourceThirdParty + `"}`
	w := do(api.InternalHandler(), "POST", "/api/v1/internal/account/link/code", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint: code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	code, _ := acctBody(t, w)["code"].(string)

	// Verify on the web: the response and the stored link must both carry thirdparty.
	w = do(api.ExternalHandler(), "POST", "/api/v1/account/link/verify", `{"code":"`+code+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := acctBody(t, w)["auth_source"]; got != authSourceThirdParty {
		t.Errorf("verify body auth_source = %v, want %q", got, authSourceThirdParty)
	}
	if got := repo.linkAuthSource[mcUUID]; got != authSourceThirdParty {
		t.Errorf("stored link auth_source = %q, want %q", got, authSourceThirdParty)
	}
}

// TestLinkStart pins the status endpoint handleClaim's 412 points at: it reports
// link state and instructions, and never mints (it has no UUID to mint against).
func TestLinkStart(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	mk := func(repo *fakeRepo) http.Handler {
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		return api.ExternalHandler()
	}

	t.Run("not linked", func(t *testing.T) {
		w := do(mk(newFakeRepo()), "POST", "/api/v1/account/link/start", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		b := acctBody(t, w)
		if b["linked"] != false {
			t.Errorf("linked = %v, want false", b["linked"])
		}
		if s, _ := b["instructions"].(string); s == "" {
			t.Error("start must return non-empty instructions")
		}
	})
	t.Run("already linked", func(t *testing.T) {
		repo := newFakeRepo()
		repo.linked["u1"] = true
		w := do(mk(repo), "POST", "/api/v1/account/link/start", "", nil)
		if b := acctBody(t, w); b["linked"] != true {
			t.Errorf("linked = %v, want true", b["linked"])
		}
	})
}

// TestAccountLinkFaceSeparation enforces the schema-forced face split: the code
// generator is internal-only, and verify/start are web-only. Crossing those
// faces must 404, not silently work.
func TestAccountLinkFaceSeparation(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: user}
	ih := api.InternalHandler()
	eh := api.ExternalHandler()

	// The generator must not exist on the web face — a logged-in user could
	// otherwise mint a code for a UUID they never proved they own.
	if w := do(eh, "POST", "/api/v1/internal/account/link/code", `{"mc_uuid":"x"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("code endpoint on external face: code = %d, want 404", w.Code)
	}
	// verify / start are web-only: they require a logged-in principal the internal
	// face never carries.
	if w := do(ih, "POST", "/api/v1/account/link/verify", `{"code":"ABCDEFGH"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("verify endpoint on internal face: code = %d, want 404", w.Code)
	}
	if w := do(ih, "POST", "/api/v1/account/link/start", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("start endpoint on internal face: code = %d, want 404", w.Code)
	}
}
