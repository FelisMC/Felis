package api

import (
	"net/http"

	"felis.lolicon.best/internal/metrics"
)

// handleLogout is public and idempotent so missing or expired cookies can still be cleared.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if val, ok := singleSessionCookie(r); ok {
		hash := hashCookie(val)
		u, uerr := a.Repo.SessionUser(r.Context(), hash, a.now())
		if err := a.Repo.RevokeSession(r.Context(), hash); err == nil && uerr == nil {
			metrics.SessionsRevokedTotal.WithLabelValues("logout").Inc()
			a.auditEntry(r, AuditEntry{Actor: u.Username, ActorUserID: u.ID, Action: "auth.logout"})
		}
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
