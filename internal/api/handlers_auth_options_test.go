package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// seedAuthOptionsAPI enables local sessions and supplies verified player and staff accounts with mail and passkey support.
func seedAuthOptionsAPI(t *testing.T) (*API, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["player"] = &StaffUser{ID: "u1", Username: "player", Email: "player@example.net", Role: "user", EmailVerified: true}
	repo.staff["boss"] = &StaffUser{ID: "a1", Username: "boss", Email: "boss@example.net", Role: "admin", EmailVerified: true}
	api := newTestAPI(repo, newFakeCluster())
	api.Passkey = &fakePasskeyVerifier{}
	api.Mailer = &captureMailer{}
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

func TestAuthOptionsEmailRequiresMailRelay(t *testing.T) {
	api, repo := seedAuthOptionsAPI(t)
	api.Mailer = nil
	repo.passkeyCreds["a"] = PasskeyCredential{ID: "a", UserID: "a1", CredentialID: "c-a1", PublicKey: "k", CreatedAt: frozenNow}
	eh := api.ExternalHandler()
	if w := do(eh, "POST", authOptionsPath, `{"email":"boss@example.net"}`, jsonHeader); w.Body.String() != `{"methods":["passkey"]}`+"\n" {
		t.Errorf("passkey account body = %q, want only passkey", w.Body.String())
	}
	if w := do(eh, "POST", authOptionsPath, `{"email":"player@example.net"}`, jsonHeader); w.Body.String() != `{"methods":[]}`+"\n" {
		t.Errorf("email-only account body = %q, want no methods", w.Body.String())
	}
}

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

func TestAuthOptionsFaceSeparation(t *testing.T) {
	api, _ := seedAuthOptionsAPI(t)
	ih := api.InternalHandler()
	if w := do(ih, "POST", authOptionsPath, `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusNotFound {
		t.Errorf("options on internal face: code = %d, want 404 (it is external-only)", w.Code)
	}
}
