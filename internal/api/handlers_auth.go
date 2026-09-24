package api

import (
	"net/http"

	"felis.lolicon.best/internal/metrics"
)

// Passwordless auth handlers (spec §B). Staff (Owner/Operator) authenticate via
// email-OTP / passkey + in-game approve on op.console; players via bind code or
// email-OTP on console. There is NO password login path. This file holds only the
// logout handler — the login doors live in handlers_auth_email.go (email OTP),
// handlers_onboard.go (bind code), and the deferred passkey-login slice.

// handleLogout revokes the presented session and clears the cookie (spec §B). It
// is mounted Public and idempotent: it reads the cookie directly, so it works even
// when the session has already expired and never errors on a missing one. Ending
// a live session is audited under its account; a dead cookie leaves no row.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		hash := hashCookie(c.Value)
		u, uerr := a.Repo.SessionUser(r.Context(), hash, a.now())
		if err := a.Repo.RevokeSession(r.Context(), hash); err == nil && uerr == nil {
			metrics.SessionsRevokedTotal.WithLabelValues("logout").Inc()
			a.auditEntry(r, AuditEntry{Actor: u.Username, ActorUserID: u.ID, Action: "auth.logout"})
		}
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
