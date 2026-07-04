package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Pre-session identifier-first discovery tests (spec §B, #71). Load-bearing properties:
//
//   - Methods reflect real state: email_otp for any resolved verified account, plus
//     passkey when a verifier is wired AND the account has >=1 enrolled credential.
//   - Existence IS disclosed: an unknown address returns an empty methods array. This
//     endpoint is the deliberate, sanctioned counter-slice to the anti-enumeration
//     login doors, so it does not disguise non-existence.
//   - Staffness is NOT disclosed: a staff email and a player email in the same
//     credential state return BYTE-IDENTICAL bodies — the highest-value guard, because a
//     role branch here would out which addresses are operators.
//   - passkey is gated on a wired verifier: options never advertises a method the finish
//     door would immediately 503.

// seedAuthOptionsAPI wires the discovery door: local sessions enabled, a verified player
// (u1) and a verified staff account (a1), and a passkey verifier wired by default.
// Callers seed passkey credentials per-test to set the credential state.
func seedAuthOptionsAPI(t *testing.T) (*API, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["player"] = &StaffUser{ID: "u1", Username: "player", Email: "player@example.net", Role: "user", EmailVerified: true}
	repo.staff["boss"] = &StaffUser{ID: "a1", Username: "boss", Email: "boss@example.net", Role: "admin", EmailVerified: true}
	api := newTestAPI(repo, newFakeCluster())
	api.Passkey = &fakePasskeyVerifier{}
	return api, repo
}

const authOptionsPath = "/api/v1/auth/options"

// optionsMethods pulls the methods array out of a 200 body as []string.
func optionsMethods(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	raw, ok := acctBody(t, w)["methods"].([]any)
	if !ok {
		t.Fatalf("body has no methods array: %s", w.Body.String())
	}
	out := make([]string, len(raw))
	for i, m := range raw {
		out[i], _ = m.(string)
	}
	return out
}

func TestAuthOptionsMethodsByState(t *testing.T) {
	t.Run("account with no passkey -> email_otp only", func(t *testing.T) {
		api, _ := seedAuthOptionsAPI(t)
		w := do(api.ExternalHandler(), "POST", authOptionsPath, `{"email":"player@example.net"}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if got := optionsMethods(t, w); !reflect.DeepEqual(got, []string{"email_otp"}) {
			t.Errorf("methods = %v, want [email_otp]", got)
		}
	})
	t.Run("account with a passkey (verifier wired) -> passkey + email_otp", func(t *testing.T) {
		api, repo := seedAuthOptionsAPI(t)
		repo.passkeyCreds["row1"] = PasskeyCredential{ID: "row1", UserID: "u1", CredentialID: "cred-1", PublicKey: "k", CreatedAt: frozenNow}
		w := do(api.ExternalHandler(), "POST", authOptionsPath, `{"email":"player@example.net"}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		// Deterministic order (passkey before email_otp) so clients and this assertion
		// can compare without sorting.
		if got := optionsMethods(t, w); !reflect.DeepEqual(got, []string{"passkey", "email_otp"}) {
			t.Errorf("methods = %v, want [passkey email_otp]", got)
		}
	})
}

// TestAuthOptionsUnknownEmail pins the sanctioned-oracle contract: an address with no
// verified account is not disguised — it returns an explicit empty array (not null), so
// the client can trust "no methods" as "no account".
func TestAuthOptionsUnknownEmail(t *testing.T) {
	api, _ := seedAuthOptionsAPI(t)
	w := do(api.ExternalHandler(), "POST", authOptionsPath, `{"email":"ghost@example.net"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := optionsMethods(t, w); len(got) != 0 {
		t.Errorf("methods = %v, want []", got)
	}
	if body := w.Body.String(); !strings.Contains(body, `"methods":[]`) {
		t.Errorf("unknown-email body = %s, want an explicit \"methods\":[] (not null)", body)
	}
}

// TestAuthOptionsDoesNotRevealStaffness is the security anchor. For each credential
// state, a staff address and a player address in the SAME state must return
// byte-identical bodies. A role branch in the handler — even one that only reordered or
// relabelled — would out which addresses are operators; this is the guard that such a
// branch can never be introduced without a red test.
func TestAuthOptionsDoesNotRevealStaffness(t *testing.T) {
	states := []struct {
		name        string
		withPasskey bool
	}{
		{"neither has a passkey", false},
		{"both have a passkey", true},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			api, repo := seedAuthOptionsAPI(t)
			if st.withPasskey {
				repo.passkeyCreds["p"] = PasskeyCredential{ID: "p", UserID: "u1", CredentialID: "c-u1", PublicKey: "k", CreatedAt: frozenNow}
				repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "a1", CredentialID: "c-a1", PublicKey: "k", CreatedAt: frozenNow}
			}
			eh := api.ExternalHandler()
			wPlayer := do(eh, "POST", authOptionsPath, `{"email":"player@example.net"}`, jsonHeader)
			wStaff := do(eh, "POST", authOptionsPath, `{"email":"boss@example.net"}`, jsonHeader)
			if wPlayer.Code != http.StatusOK || wStaff.Code != http.StatusOK {
				t.Fatalf("codes = %d/%d, want 200/200", wPlayer.Code, wStaff.Code)
			}
			if wPlayer.Body.String() != wStaff.Body.String() {
				t.Errorf("staff/player bodies differ — options reveals staffness:\n player: %s\n staff:  %s",
					wPlayer.Body.String(), wStaff.Body.String())
			}
		})
	}
}

// TestAuthOptionsPasskeyRequiresWiredVerifier: the account HAS an enrolled passkey, but
// no verifier is wired (a.Passkey == nil). Both login halves 503 passkey_unavailable in
// that state, so options must NOT advertise passkey — it would be a dead offer.
func TestAuthOptionsPasskeyRequiresWiredVerifier(t *testing.T) {
	api, repo := seedAuthOptionsAPI(t)
	repo.passkeyCreds["row1"] = PasskeyCredential{ID: "row1", UserID: "u1", CredentialID: "cred-1", PublicKey: "k", CreatedAt: frozenNow}
	api.Passkey = nil
	w := do(api.ExternalHandler(), "POST", authOptionsPath, `{"email":"player@example.net"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := optionsMethods(t, w); !reflect.DeepEqual(got, []string{"email_otp"}) {
		t.Errorf("methods = %v, want [email_otp] (passkey must not be offered without a wired verifier)", got)
	}
}

func TestAuthOptionsGates(t *testing.T) {
	t.Run("local auth disabled -> 403", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), newFakeCluster()) // no LocalAuthEnabledKey: fails closed
		api.Passkey = &fakePasskeyVerifier{}
		w := do(api.ExternalHandler(), "POST", authOptionsPath, `{"email":"player@example.net"}`, jsonHeader)
		if w.Code != http.StatusForbidden || decodeErr(t, w) != "local_auth_disabled" {
			t.Errorf("code = %d body %s, want 403 local_auth_disabled", w.Code, w.Body.String())
		}
	})
	t.Run("non-JSON content type -> 415", func(t *testing.T) {
		api, _ := seedAuthOptionsAPI(t)
		eh := api.ExternalHandler()
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			if w := do(eh, "POST", authOptionsPath, `{"email":"player@example.net"}`, ctHeader(ct)); w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("Content-Type %q: code = %d, want 415", ct, w.Code)
			}
		}
	})
	t.Run("bad or unknown-field body -> 400", func(t *testing.T) {
		bad := map[string]string{
			"missing email": `{}`,
			"empty email":   `{"email":""}`,
			"no at-sign":    `{"email":"notanemail"}`,
			"unknown field": `{"email":"player@example.net","x":1}`,
		}
		api, _ := seedAuthOptionsAPI(t)
		eh := api.ExternalHandler()
		for name, body := range bad {
			if w := do(eh, "POST", authOptionsPath, body, jsonHeader); w.Code != http.StatusBadRequest {
				t.Errorf("%s: code = %d, want 400 (%s)", name, w.Code, w.Body.String())
			}
		}
	})
}

// TestAuthOptionsFaceSeparation: the route is external-only (registered in
// externalAPIRoutes), so the internal face must 404 it.
func TestAuthOptionsFaceSeparation(t *testing.T) {
	api, _ := seedAuthOptionsAPI(t)
	ih := api.InternalHandler()
	if w := do(ih, "POST", authOptionsPath, `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("options on internal face: code = %d, want 404 (it is external-only)", w.Code)
	}
}
