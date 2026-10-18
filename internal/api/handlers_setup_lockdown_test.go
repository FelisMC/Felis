package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdvClaim403MechanismIsRequireOnboarded(t *testing.T) {
	api, repo := seedBindAPI(t)
	mintBindCode(t, api, repo, "DEMO2345", bindTestUUID, authSourceMojang)

	w := do(api.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"DEMO2345"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("bind = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	cookie := map[string]string{"Cookie": sessionCookieName + "=" + sessionCookieValue(t, w)}
	userID := repo.staff[bindTestUUID].ID

	if u := repo.staff[bindTestUUID]; u.EmailVerified {
		t.Fatalf("bind-created player has EmailVerified=true; lockdown premise is wrong")
	}
	if creds, _ := repo.PasskeyCredentialsForUser(t.Context(), userID); len(creds) != 0 {
		t.Fatalf("bind-created player already has %d passkeys; lockdown premise is wrong", len(creds))
	}

	cl := api.Cluster.(*fakeCluster)
	cl.byName["demo2"] = &ServerInfo{Name: "demo2", Subdomain: "demo2", Phase: "Stopped"}
	repo.claimOK["demo2"] = true
	repo.quota[userID] = true

	if got := do(api.ExternalHandler(), "GET", "/api/v1/me/servers", "", cookie); got.Code != http.StatusOK {
		t.Fatalf("/me/servers = %d, want 200 (%s)", got.Code, got.Body.String())
	}

	routes := []struct{ method, path string }{
		{"POST", "/api/v1/servers/demo2/claim"},
		{"POST", "/api/v1/servers/demo2/wake"},
		{"GET", "/api/v1/servers/demo2/status"},
	}
	for _, tc := range routes {
		got := do(api.ExternalHandler(), tc.method, tc.path, "", cookie)
		if got.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d, want 403 (%s)", tc.method, tc.path, got.Code, got.Body.String())
		}
		// The DISCRIMINATOR: setup_required proves requireOnboarded fired. If the
		// 403 came from isOwnerOrAdmin the code would be "forbidden".
		if c := errCode(got.Body.Bytes()); c != "setup_required" {
			t.Fatalf("%s %s 403 code = %q, want setup_required (%s)",
				tc.method, tc.path, c, got.Body.String())
		}
	}

	// One bit: enroll a passkey. Nothing else about the request changes.
	repo.passkeyCreds["pk1"] = PasskeyCredential{ID: "pk1", UserID: userID, CredentialID: "c1"}

	// Claim now reaches its handler and succeeds.
	if got := do(api.ExternalHandler(), "POST", "/api/v1/servers/demo2/claim", "", cookie); got.Code != http.StatusOK {
		t.Fatalf("post-passkey claim = %d %q, want 200 (%s)", got.Code, errCode(got.Body.Bytes()), got.Body.String())
	}
	// The fake claim records only the row count; mirror the ownership write before testing setup behavior.
	repo.byName["demo2"] = &ServerRecord{Name: "demo2", Subdomain: "demo2", OwnerID: userID}

	// With ownership recorded, wake and status pass authorizeWake/isOwnerOrAdmin —
	// proving the earlier 403s were the lockdown, not the ownership check.
	for _, tc := range routes[1:] {
		got := do(api.ExternalHandler(), tc.method, tc.path, "", cookie)
		if got.Code == http.StatusForbidden {
			t.Fatalf("%s %s still 403 for the owner after passkey enrollment (%s)",
				tc.method, tc.path, got.Body.String())
		}
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
		// The live demo2 player: bind-onboarded, no email, no passkey.
		{"session player, unverified, no passkey", &Principal{UserID: "u1", ViaSession: true, EmailVerified: false}, http.StatusForbidden},
		{"session player, email verified", &Principal{UserID: "u1", ViaSession: true, EmailVerified: true}, passed},
		{"session player, unverified but passkey enrolled", &Principal{UserID: "has-pk", ViaSession: true, EmailVerified: false}, passed},
		// Admin Zero-Trust path carries no session flag; internal face carries no principal.
		{"non-session principal", &Principal{UserID: "a1", ViaSession: false, EmailVerified: false}, passed},
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
		&Principal{UserID: "has-pk", ViaSession: true, EmailVerified: false}))
	w := httptest.NewRecorder()
	h(w, r)
	if ran {
		t.Fatal("the handler ran although the onboarding check could not be made")
	}
	if w.Code != http.StatusServiceUnavailable || errCode(w.Body.Bytes()) != "auth_unavailable" {
		t.Fatalf("got %d %s, want 503 auth_unavailable", w.Code, w.Body.String())
	}
}
