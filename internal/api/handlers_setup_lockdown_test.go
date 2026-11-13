package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBindCodePlayerUsesThePanelWithoutAPasskey(t *testing.T) {
	api, repo := seedBindAPI(t)
	mintBindCode(t, api, repo, "DEMO2345", bindTestUUID, authSourceMojang)

	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"DEMO2345"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("bind = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	cookie := map[string]string{"Cookie": sessionCookieName + "=" + sessionCookieValue(t, w)}
	userID := repo.staff[bindTestUUID].ID
	if u := repo.staff[bindTestUUID]; u.EmailVerified {
		t.Fatalf("bind-created player has EmailVerified=true; the premise is wrong")
	}

	cl := api.Cluster.(*fakeCluster)
	cl.byName["demo2"] = &ServerInfo{Name: "demo2", Subdomain: "demo2", Phase: "Stopped"}
	repo.claimOK["demo2"] = true
	repo.quota[userID] = true

	if got := do(api.ExternalHandler(), "POST", "/api/v1/servers/demo2/claim", "", cookie); got.Code != http.StatusOK {
		t.Fatalf("claim by a player who signed in from the game = %d %q, want 200 (%s)",
			got.Code, errCode(got.Body.Bytes()), got.Body.String())
	}
}

func TestAdvRequireOnboardedPredicate(t *testing.T) {
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	repo.passkeyCreds["pk-enrolled"] = PasskeyCredential{ID: "pk-enrolled", UserID: "has-pk", CredentialID: "c1"}

	const passed = http.StatusTeapot // the wrapped handler ran
	h := api.requireOnboarded(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(passed) })

	for _, tc := range []struct {
		name string
		p    *Principal
		want int
	}{
		{"session player, unverified, no passkey", &Principal{UserID: "u1", Role: "user", ViaSession: true}, passed},
		{"session owner, unverified, no passkey", &Principal{UserID: "u1", Role: "owner", ViaSession: true}, http.StatusForbidden},
		{"api key of an admin, unverified, no passkey", &Principal{UserID: "u1", Role: "admin", APIKey: "k1"}, http.StatusForbidden},
		{"session owner, email verified", &Principal{UserID: "u1", Role: "owner", ViaSession: true, EmailVerified: true}, passed},
		{"session admin, unverified but passkey enrolled", &Principal{UserID: "has-pk", Role: "admin", ViaSession: true}, passed},
		// Admin Zero-Trust path carries no session flag; internal face carries no principal.
		{"non-session principal", &Principal{UserID: "a1", Role: "admin"}, passed},
		{"nil principal (internal face)", nil, passed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/servers/demo2/wake", nil)
			if tc.p != nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal, tc.p))
			}
			w := httptest.NewRecorder()
			h(w, r)
			if w.Code != tc.want {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusForbidden && errCode(w.Body.Bytes()) != "setup_required" {
				t.Fatalf("403 code = %q, want setup_required", errCode(w.Body.Bytes()))
			}
		})
	}
}

// credsDown is a store whose passkey lookup fails, as during a Postgres outage.
type credsDown struct{ *fakeRepo }

func (credsDown) PasskeyCredentialsForUser(context.Context, string) ([]PasskeyCredential, error) {
	return nil, errors.New("connection refused")
}

func TestRequireOnboardedReportsAStoreOutage(t *testing.T) {
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())
	api.Repo = credsDown{repo}
	ran := false
	h := api.requireOnboarded(func(http.ResponseWriter, *http.Request) { ran = true })

	r := httptest.NewRequest("POST", "/api/v1/servers/demo2/wake", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal,
		&Principal{UserID: "has-pk", Role: "owner", ViaSession: true}))
	w := httptest.NewRecorder()
	h(w, r)
	if ran {
		t.Fatal("the handler ran although the onboarding check could not be made")
	}
	if w.Code != http.StatusServiceUnavailable || errCode(w.Body.Bytes()) != "auth_unavailable" {
		t.Fatalf("got %d %s, want 503 auth_unavailable", w.Code, w.Body.String())
	}
}
