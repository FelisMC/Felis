package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// uvCases are the ways an assertion can lack user verification (#9): the credential
// was bound presence-only (a pre-0009 row, or a future UV=preferred enrollment), this
// assertion itself did not verify the user, or it names a credential the account does
// not hold. Each must be refused at every door that grants or confirms a session.
var uvCases = []struct {
	name       string
	storedUV   bool
	assertUV   bool
	assertCred string
}{
	{"credential bound without UV", false, true, ""},
	{"assertion without UV", true, false, ""},
	{"credential not bound to the account", true, true, "cred-elsewhere"},
}

func TestPasskeyLoginRefusesUnverifiedUser(t *testing.T) {
	for _, tc := range uvCases {
		t.Run(tc.name, func(t *testing.T) {
			api, repo, v := seedLoginPasskeyAPI(t)
			row := repo.passkeyCreds["row1"]
			row.UserVerified = tc.storedUV
			repo.passkeyCreds["row1"] = row
			v.assertion = VerifiedAssertion{CredentialID: "cred-1", UserVerified: tc.assertUV, SignCount: 3}
			if tc.assertCred != "" {
				v.assertion.CredentialID = tc.assertCred
			}
			plantLoginChallenge(repo, "live", frozenNow.Add(passkeyChallengeTTL))

			w := do(api.ExternalHandler(), "POST", "/api/v1/auth/passkey/login/finish", finishBody("player@example.net", "YXNzZXJ0"), jsonHeader)
			assertUVRefused(t, w, repo, "player")
		})
	}
}

func TestPasskeyDiscoverableLoginRefusesUnverifiedUser(t *testing.T) {
	for _, tc := range uvCases {
		t.Run(tc.name, func(t *testing.T) {
			api, repo, v := seedDiscoverableLoginAPI(t)
			row := repo.passkeyCreds["row1"]
			row.UserVerified = tc.storedUV
			repo.passkeyCreds["row1"] = row
			v.assertion = VerifiedAssertion{CredentialID: "cred-1", UserVerified: tc.assertUV, SignCount: 3}
			if tc.assertCred != "" {
				v.assertion.CredentialID = tc.assertCred
			}
			eh := api.ExternalHandler()

			loginID := beginDiscoverableLogin(t, eh)
			w := do(eh, "POST", "/api/v1/auth/passkey/login/discoverable/finish",
				`{"login_id":"`+loginID+`","assertion":{"id":"cred-1","type":"public-key"}}`, jsonHeader)
			assertUVRefused(t, w, repo, "player")
		})
	}
}

func TestReauthByPasskeyRefusesUnverifiedUser(t *testing.T) {
	for _, tc := range uvCases {
		t.Run(tc.name, func(t *testing.T) {
			f := reauthFixture(t)
			pv := f.api.Passkey.(*fakePasskeyVerifier)
			f.repo.passkeyCreds["row1"] = PasskeyCredential{ID: "row1", UserID: "u1", CredentialID: "cred-1",
				UserVerified: tc.storedUV, CreatedAt: frozenNow}
			pv.assertion = VerifiedAssertion{CredentialID: "cred-1", UserVerified: tc.assertUV, SignCount: 3}
			if tc.assertCred != "" {
				pv.assertion.CredentialID = tc.assertCred
			}
			if w := do(f.eh, "POST", "/api/v1/account/reauth/passkey/begin", "", asCookie(laptopTok)); w.Code != http.StatusOK {
				t.Fatalf("begin = %d (%s)", w.Code, w.Body.String())
			}
			f.repo.audits = nil
			w := do(f.eh, "POST", "/api/v1/account/reauth/passkey/finish", `{"assertion":{"id":"cred-1"}}`, jsonCookie(laptopTok))
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "passkey_login_invalid" {
				t.Fatalf("finish = %d (%s), want 400 passkey_login_invalid", w.Code, w.Body.String())
			}
			if !f.reauthAt(laptopTok).IsZero() {
				t.Fatal("an unverified assertion marked the session reauthenticated")
			}
			if got := f.repo.passkeyCreds["row1"]; got.SignCount != 0 || got.LastUsedAt != nil {
				t.Errorf("refusal advanced the credential: SignCount=%d LastUsedAt=%v", got.SignCount, got.LastUsedAt)
			}
			if n := len(f.repo.audits); n != 1 || f.repo.audits[0].Action != "auth.passkey_uv_rejected" {
				t.Fatalf("want exactly 1 auth.passkey_uv_rejected audit, got %+v", f.repo.audits)
			}
		})
	}
}

// assertUVRefused checks a sign-in door's refusal: the opaque envelope every finish
// failure shares, no session, the stored credential untouched, and one distinct audit
// under the account.
func assertUVRefused(t *testing.T, rec *httptest.ResponseRecorder, repo *fakeRepo, actor string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest || decodeErr(t, rec) != "passkey_login_invalid" {
		t.Fatalf("finish = %d (%s), want 400 passkey_login_invalid", rec.Code, rec.Body.String())
	}
	if len(repo.sessions) != 0 || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("refusal minted a session: sessions=%d cookies=%v", len(repo.sessions), rec.Result().Cookies())
	}
	if got := repo.passkeyCreds["row1"]; got.SignCount != 0 || got.LastUsedAt != nil {
		t.Errorf("refusal advanced the credential: SignCount=%d LastUsedAt=%v", got.SignCount, got.LastUsedAt)
	}
	if n := len(repo.audits); n != 1 || repo.audits[0].Action != "auth.passkey_uv_rejected" || repo.audits[0].Actor != actor {
		t.Fatalf("want exactly 1 auth.passkey_uv_rejected audit by %s, got %+v", actor, repo.audits)
	}
}
