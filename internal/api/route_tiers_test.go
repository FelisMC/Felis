package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Each tier a route sets is one more gate in front of its handler: a route marked
// both Owner and Admin still refuses an admin who is not the owner.
func TestBuildFaceStacksTierGates(t *testing.T) {
	a := newTestAPI(newFakeRepo(), newFakeCluster())
	ran := false
	ok := func(w http.ResponseWriter, r *http.Request) { ran = true }
	as := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := &Principal{UserID: "u1", Role: r.Header.Get("X-Test-Role"), ViaAdminAccess: true, EmailVerified: true}
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal, p)))
		})
	}
	h := a.buildFace("external", []apiRoute{{Method: "GET", Pattern: "/api/v1/tiered", Owner: true, Admin: true, h: ok}}, as)

	for _, c := range []struct {
		role string
		code int
	}{
		{"admin", http.StatusForbidden},
		{"user", http.StatusForbidden},
		{"owner", http.StatusOK},
	} {
		ran = false
		r := httptest.NewRequest("GET", "/api/v1/tiered", nil)
		r.Header.Set("X-Test-Role", c.role)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.code || ran != (c.code == http.StatusOK) {
			t.Errorf("%s: status %d, handler ran %v; want %d", c.role, w.Code, ran, c.code)
		}
	}
}
