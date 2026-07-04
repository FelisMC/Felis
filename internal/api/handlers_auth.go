package api

import "net/http"

// Passwordless auth handlers (spec §B). Staff (Owner/Operator) authenticate via
// email-OTP / passkey + in-game approve on op.console; players via bind code or
// email-OTP on console. There is NO password login path. This file holds only the
// logout handler — the login doors live in handlers_auth_email.go (email OTP),
// handlers_onboard.go (bind code), and the deferred passkey-login slice.

// handleLogout revokes the presented session and clears the cookie (spec §B). It
// is mounted Public and idempotent: it reads the cookie directly, so it works even
// when the session has already expired and never errors on a missing one.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		_ = a.Repo.RevokeSession(r.Context(), hashCookie(c.Value))
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
