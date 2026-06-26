package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// withRequestID assigns a request id (honoring an inbound X-Request-Id) and
// echoes it on the response and into the context for the error envelope.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withRecover turns a panicking handler into a 500 envelope instead of a
// dropped connection.
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeError(w, r, newError(http.StatusInternalServerError, "panic", "internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requireInternal enforces service-token auth for the internal face. It never
// applies Zero Trust (spec §14 red line).
func (a *API) requireInternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.Internal.Authenticate(r); err != nil {
			writeError(w, r, errUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireExternal enforces Access-JWT auth for the external face and stashes the
// resolved Principal in the request context.
func (a *API) requireExternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.External.Authenticate(r)
		if err != nil || p == nil {
			writeError(w, r, errUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyPrincipal, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// adminOnly gates an external-face handler on the admin Zero-Trust path. The
// Access middleware has already authenticated; this enforces that admin-tier
// operations both carry role=admin and arrived via admin.* (spec §14).
func (a *API) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := principalFromContext(r.Context()); !p.IsAdmin() {
			writeError(w, r, errForbidden)
			return
		}
		next(w, r)
	}
}

// lockdownDuringPasswordChange fences a staff principal that still owes a
// first-login password change to the change-password surface (spec §B). It is the
// default-deny half of the lockdown: buildFace wraps every authenticated route
// with it except the AllowDuringPasswordChange opt-outs, so a half-onboarded
// account can do nothing but change its password, log out, or read /me. It is
// nil-principal safe (the internal face sets no Principal), so it passes such
// requests straight through and only ever acts on the external face.
func (a *API) lockdownDuringPasswordChange(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := principalFromContext(r.Context()); p != nil && p.MustChangePassword {
			writeError(w, r, errPasswordChangeRequired)
			return
		}
		next(w, r)
	}
}

// newRequestID returns a short random hex id. crypto/rand never fails on the
// platforms we target; on the impossible error path we fall back to a constant
// so a request still gets a (non-unique) id rather than crashing.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}
