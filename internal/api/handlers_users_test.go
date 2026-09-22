package api

import (
	"net/http"
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
	})
	t.Run("delete refused", func(t *testing.T) {
		w := do(eh, "DELETE", "/api/v1/users/usr-owner2", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("delete owner: code = %d body %s, want 403", w.Code, w.Body.String())
		}
	})
	t.Run("disable refused", func(t *testing.T) {
		w := do(eh, "POST", "/api/v1/users/usr-owner2/disable", `{"disabled":true}`, jsonHeader)
		if w.Code != http.StatusForbidden {
			t.Fatalf("disable owner: code = %d body %s, want 403", w.Code, w.Body.String())
		}
	})
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
