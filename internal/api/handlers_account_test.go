package api

import (
	"encoding/json"
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

func TestAccountLinkVertical(t *testing.T) {
	const mcUUID = "11111111-1111-1111-1111-111111111111"
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}

	repo := newFakeRepo()
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
	if len(code) != LinkCodeLen {
		t.Fatalf("minted code %q: len = %d, want %d", code, len(code), LinkCodeLen)
	}

	// 2) the player submits the code on the panel (external face).
	w = do(eh, "POST", "/api/v1/account/link/verify", `{"code":"`+code+`"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if b := acctBody(t, w); b["linked"] != true || b["mc_uuid"] != mcUUID || b["auth_source"] != authSourceMojang {
		t.Fatalf("verify body = %v, want linked:true mc_uuid:%s auth_source:%s", b, mcUUID, authSourceMojang)
	}
	if n := len(repo.audits); n != 1 || repo.audits[0].Action != "account.link" || repo.audits[0].Actor != "u1@example.net" {
		t.Fatalf("link audit not written as expected: %+v", repo.audits)
	}

	if w := do(eh, "POST", "/api/v1/servers/survival/claim", "", nil); w.Code != http.StatusOK {
		t.Fatalf("post-link claim: code = %d body %s, want 200", w.Code, w.Body.String())
	}
}

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
		if want := api.now().Add(LinkCodeTTL); !rec.expiresAt.Equal(want) {
			t.Errorf("expiresAt = %v, want %v", rec.expiresAt, want)
		}
		for _, c := range code {
			if !strings.ContainsRune(LinkCodeAlphabet, c) {
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
		if _, ok := repo.linkCodes["FRESHCOD"]; !ok {
			t.Error("a conflicting verify must not consume the code")
		}
	})
}

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

func TestAccountLinkFaceSeparation(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: user}
	ih := api.InternalHandler()
	eh := api.ExternalHandler()

	if w := do(eh, "POST", "/api/v1/internal/account/link/code", `{"mc_uuid":"x"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("code endpoint on external face: code = %d, want 404", w.Code)
	}
	if w := do(ih, "POST", "/api/v1/account/link/verify", `{"code":"ABCDEFGH"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("verify endpoint on internal face: code = %d, want 404", w.Code)
	}
	if w := do(ih, "POST", "/api/v1/account/link/start", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("start endpoint on internal face: code = %d, want 404", w.Code)
	}
}
