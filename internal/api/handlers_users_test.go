package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The owner row is the one identity the panel may never demote, delete, or
// disable (migration 0011) — only the local break-glass console resets it.
// These guards became load-bearing the moment provisioning started actually
// writing role='owner'; before that the owner tier was simply unreachable, so
// nothing could reach them.
func TestOwnerAccountProtectedFromPanelMutations(t *testing.T) {
	owner := &Principal{UserID: "usr-root", Role: "owner", ViaAdminAccess: true}
	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "usr-root", Username: "root", Role: "owner"})
	repo.seedUser(UserView{ID: "usr-owner2", Username: "spare-owner", Role: "owner"})
	repo.seedUser(UserView{ID: "u2", Username: "alice", Role: "user"})
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: owner}
	eh := api.ExternalHandler()

	t.Run("role change refused", func(t *testing.T) {
		w := do(eh, "PATCH", "/api/v1/users/usr-owner2", `{"role":"admin"}`, jsonHeader)
		if w.Code != http.StatusForbidden {
			t.Fatalf("demote owner: code = %d body %s, want 403", w.Code, w.Body.String())
		}
		if got := decodeErr(t, w); got != "owner_protected" {
			t.Fatalf("demote owner: error code = %q, want owner_protected", got)
		}
	})
	t.Run("delete refused", func(t *testing.T) {
		w := do(eh, "DELETE", "/api/v1/users/usr-owner2", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("delete owner: code = %d body %s, want 403", w.Code, w.Body.String())
		}
		if got := decodeErr(t, w); got != "owner_protected" {
			t.Fatalf("delete owner: error code = %q, want owner_protected", got)
		}
	})
	t.Run("disable refused", func(t *testing.T) {
		w := do(eh, "POST", "/api/v1/users/usr-owner2/disable", `{"disabled":true}`, jsonHeader)
		if w.Code != http.StatusForbidden {
			t.Fatalf("disable owner: code = %d body %s, want 403", w.Code, w.Body.String())
		}
		if got := decodeErr(t, w); got != "owner_protected" {
			t.Fatalf("disable owner: error code = %q, want owner_protected", got)
		}
	})
	// The caller's own row is refused as self_protected even though it is also
	// an owner: that is the reason the admin can act on.
	for _, tc := range []struct{ name, method, path, body string }{
		{"own role", "PATCH", "/api/v1/users/usr-root", `{"role":"admin"}`},
		{"own delete", "DELETE", "/api/v1/users/usr-root", ""},
		{"own disable", "POST", "/api/v1/users/usr-root/disable", `{"disabled":true}`},
	} {
		t.Run(tc.name+" refused as self", func(t *testing.T) {
			var h map[string]string
			if tc.body != "" {
				h = jsonHeader
			}
			w := do(eh, tc.method, tc.path, tc.body, h)
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d body %s, want 403", w.Code, w.Body.String())
			}
			if got := decodeErr(t, w); got != "self_protected" {
				t.Fatalf("error code = %q, want self_protected", got)
			}
		})
	}
	t.Run("email edits on an owner stay allowed", func(t *testing.T) {
		w := do(eh, "PATCH", "/api/v1/users/usr-owner2", `{"email":"root2@example.net"}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("edit owner email: code = %d body %s, want 200", w.Code, w.Body.String())
		}
	})
	t.Run("control: a normal user can still be promoted", func(t *testing.T) {
		w := do(eh, "PATCH", "/api/v1/users/u2", `{"role":"admin"}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("promote user: code = %d body %s, want 200", w.Code, w.Body.String())
		}
	})
}

// The caller's own email and passkeys are ways into the caller's account, so
// changing them through /users/{id} takes the same recent reauth as through
// /account. Without it a stolen owner session could strip both here, and with no
// factor left to guard, /account/passkey/register/begin would let it plant its own.
func TestOwnSignInFactorsNeedReauthUnderUsers(t *testing.T) {
	owner := &Principal{UserID: "usr-root", Role: "owner", Email: "root@example.net",
		ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "usr-root", Username: "root", Email: "root@example.net", Role: "owner", EmailVerified: true})
	repo.seedUser(UserView{ID: "u2", Username: "alice", Email: "alice@example.net", Role: "user"})
	repo.passkeyCreds["pk-root"] = PasskeyCredential{ID: "pk-root", UserID: "usr-root", CredentialID: "c-root", UserVerified: true, CreatedAt: frozenNow}
	api := newTestAPI(repo, newFakeCluster())
	api.Mailer = &captureMailer{}
	api.External = staticExternal{p: owner}
	eh := api.ExternalHandler()
	send := func(method, path, body string) *httptest.ResponseRecorder {
		if body == "" {
			return do(eh, method, path, "", nil)
		}
		return do(eh, method, path, body, jsonHeader)
	}

	own := []struct {
		name, method, path, body string
		untouched                func() bool
	}{
		{"own email", "PATCH", "/api/v1/users/usr-root", `{"email":"thief@example.net"}`, func() bool {
			d, err := repo.UserDetail(context.Background(), "usr-root")
			return err == nil && d.Email == "root@example.net"
		}},
		{"own passkeys", "DELETE", "/api/v1/users/usr-root/passkeys", "", func() bool {
			_, ok := repo.passkeyCreds["pk-root"]
			return ok
		}},
	}
	for _, tc := range own {
		t.Run(tc.name+" refused without a recent reauth", func(t *testing.T) {
			w := send(tc.method, tc.path, tc.body)
			if w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
				t.Fatalf("code = %d body %s, want 403 reauth_required", w.Code, w.Body.String())
			}
			if !tc.untouched() {
				t.Fatal("the change went through despite the refusal")
			}
		})
	}
	// Another account's factors are the owner's to manage, and a username is no way in.
	for _, tc := range []struct{ name, method, path, body string }{
		{"another user's email", "PATCH", "/api/v1/users/u2", `{"email":"alice@new.example"}`},
		{"another user's passkeys", "DELETE", "/api/v1/users/u2/passkeys", ""},
		{"own username", "PATCH", "/api/v1/users/usr-root", `{"username":"root2"}`},
	} {
		t.Run("control: "+tc.name+" needs no reauth", func(t *testing.T) {
			if w := send(tc.method, tc.path, tc.body); w.Code != http.StatusOK {
				t.Fatalf("code = %d body %s, want 200", w.Code, w.Body.String())
			}
		})
	}
	owner.ReauthAt = api.now()
	for _, tc := range own {
		t.Run(tc.name+" allowed after a reauth", func(t *testing.T) {
			if w := send(tc.method, tc.path, tc.body); w.Code != http.StatusOK {
				t.Fatalf("code = %d body %s, want 200", w.Code, w.Body.String())
			}
			if tc.untouched() {
				t.Fatal("the change did not go through")
			}
		})
	}
}

// The user-scoped admin sub-resources (quotas, account links) must answer 404
// for an unknown user id. Before the requireLiveUser guard the quota upsert and
// the link insert reached the users(id) foreign key and surfaced as an opaque
// 500 (found live against the drill cluster, audit #30), and the quotas read
// answered a zero-value "unlimited" view as if the id existed.
func TestAdminSubresourcesRequireLiveUser(t *testing.T) {
	owner := &Principal{UserID: "usr-root", Role: "owner", ViaAdminAccess: true}
	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "usr-root", Username: "root", Role: "owner"})
	repo.seedUser(UserView{ID: "u2", Username: "alice", Role: "user"})
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: owner}
	eh := api.ExternalHandler()

	t.Run("quotas read of an unknown user", func(t *testing.T) {
		w := do(eh, "GET", "/api/v1/users/usr-nope/quotas", "", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d body %s, want 404", w.Code, w.Body.String())
		}
	})
	t.Run("quotas write of an unknown user", func(t *testing.T) {
		w := do(eh, "PUT", "/api/v1/users/usr-nope/quotas", `{"max_servers":1}`, jsonHeader)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d body %s, want 404", w.Code, w.Body.String())
		}
	})
	t.Run("account link of an unknown user", func(t *testing.T) {
		w := do(eh, "POST", "/api/v1/users/usr-nope/links",
			`{"mc_uuid":"22222222-3333-4444-5555-666666666666"}`, jsonHeader)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d body %s, want 404", w.Code, w.Body.String())
		}
	})
	t.Run("control: a live user still accepts both writes", func(t *testing.T) {
		w := do(eh, "PUT", "/api/v1/users/u2/quotas", `{"max_servers":2}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("set quotas: code = %d body %s, want 200", w.Code, w.Body.String())
		}
		w = do(eh, "POST", "/api/v1/users/u2/links",
			`{"mc_uuid":"22222222-3333-4444-5555-666666666677"}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("link: code = %d body %s, want 200", w.Code, w.Body.String())
		}
	})
}

// The quotas form replaces all four caps at once, so an owner can lift a cap they
// set: a missing or null field is unlimited, 0 grants none of it. Before, a null
// field meant "leave it", the panel sent null for every emptied box, and a cap once
// set could only be moved, never removed; a negative one was written as is.
func TestSetQuotasReplacesAllCaps(t *testing.T) {
	owner := &Principal{UserID: "usr-root", Role: "owner", ViaAdminAccess: true}
	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "usr-root", Username: "root", Role: "owner"})
	repo.seedUser(UserView{ID: "u2", Username: "alice", Role: "user"})
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: owner}
	eh := api.ExternalHandler()

	caps := func() string {
		t.Helper()
		w := do(eh, "GET", "/api/v1/users/u2/quotas", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("get quotas: code = %d body %s", w.Code, w.Body.String())
		}
		return strings.TrimSpace(w.Body.String())
	}
	put := func(body string, want int) {
		t.Helper()
		w := do(eh, "PUT", "/api/v1/users/u2/quotas", body, jsonHeader)
		if w.Code != want {
			t.Fatalf("PUT %s: code = %d body %s, want %d", body, w.Code, w.Body.String(), want)
		}
	}

	put(`{"max_servers":0,"max_cpu_milli":2000,"max_memory_mb":4096,"max_storage_gb":20}`, http.StatusOK)
	if got, want := caps(), `{"user_id":"u2","max_servers":0,"max_cpu_milli":2000,"max_memory_mb":4096,"max_storage_gb":20}`; got != want {
		t.Fatalf("after setting every cap: %s, want %s", got, want)
	}
	// What the panel sends after the owner empties two boxes.
	put(`{"max_servers":null,"max_cpu_milli":1000,"max_memory_mb":null,"max_storage_gb":20}`, http.StatusOK)
	if got, want := caps(), `{"user_id":"u2","max_cpu_milli":1000,"max_storage_gb":20}`; got != want {
		t.Fatalf("after emptying two boxes: %s, want %s", got, want)
	}
	put(`{}`, http.StatusOK)
	if got, want := caps(), `{"user_id":"u2"}`; got != want {
		t.Fatalf("after lifting every cap: %s, want %s", got, want)
	}

	put(`{"max_servers":3}`, http.StatusOK)
	for _, body := range []string{
		`{"max_servers":-1}`,
		`{"max_servers":1,"max_storage_gb":-5}`,
		`{"max_memory_mb":2147483648}`,
		`{"max_cpu_milli":1.5}`,
	} {
		put(body, http.StatusBadRequest)
	}
	if got, want := caps(), `{"user_id":"u2","max_servers":3}`; got != want {
		t.Fatalf("a refused write changed the caps: %s, want %s", got, want)
	}
	w := do(eh, "PUT", "/api/v1/users/u2/quotas", `{"max_storage_gb":-5}`, jsonHeader)
	if !strings.Contains(w.Body.String(), `"invalid_quota"`) || !strings.Contains(w.Body.String(), "max_storage_gb") {
		t.Fatalf("a negative cap should name the field: %s", w.Body.String())
	}
	put(`{"max_memory_mb":2147483647}`, http.StatusOK)
}
