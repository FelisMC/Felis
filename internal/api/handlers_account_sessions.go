package api

import (
	"errors"
	"log"
	"net/http"

	"felis.lolicon.best/internal/metrics"
)

func (a *API) handleListMySessions(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	sessions, err := a.Repo.ListUserSessions(r.Context(), p.UserID, a.now())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if sessions == nil {
		sessions = []SessionView{}
	}
	if cur := callerSessionHash(r, p); cur != "" {
		for i := range sessions {
			sessions[i].Current = sessions[i].TokenHash == cur
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// handleRevokeMySession returns 404 for all non-owned sessions and clears the cookie when revoking the current one.
func (a *API) handleRevokeMySession(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	hash := r.PathValue("hash")
	if err := a.Repo.RevokeUserSession(r.Context(), p.UserID, hash); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "session_not_found",
				"that session has already ended or is not one of yours"))
			return
		}
		writeError(w, r, err)
		return
	}
	current := hash == callerSessionHash(r, p)
	if current {
		clearSessionCookie(w)
	}
	metrics.SessionsRevokedTotal.WithLabelValues("self").Inc()
	a.audit(r, "account.session.revoked", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "signed_out": current})
}

func (a *API) handleRevokeMyOtherSessions(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	n, err := a.Repo.RevokeOtherUserSessions(r.Context(), p.UserID, callerSessionHash(r, p))
	if err != nil {
		writeError(w, r, err)
		return
	}
	metrics.SessionsRevokedTotal.WithLabelValues("self").Add(float64(n))
	a.audit(r, "account.session.revoked_others", "")
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

// revokeOtherSessionsAfter runs after a committed factor change; failure is logged without making the completed change appear retryable.
func (a *API) revokeOtherSessionsAfter(r *http.Request, change string) {
	p := principalFromContext(r.Context())
	n, err := a.Repo.RevokeOtherUserSessions(r.Context(), p.UserID, callerSessionHash(r, p))
	if err != nil {
		log.Printf("api: sign out other sessions after %s (request_id=%s): %v", change, requestIDFromContext(r.Context()), err)
		return
	}
	if n > 0 {
		metrics.SessionsRevokedTotal.WithLabelValues("security").Add(float64(n))
	}
}

func callerSessionHash(r *http.Request, p *Principal) string {
	if p == nil || !p.ViaSession {
		return ""
	}
	return currentSessionHash(r)
}
