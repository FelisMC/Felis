package api

import (
	"errors"
	"log"
	"net/http"

	"felis.lolicon.best/internal/metrics"
)

// The account holder's own sessions: every device signed in to the account,
// which one is making this request, and a way to sign any of them out. The admin
// routes in handlers_users.go read and revoke the same rows for any user.

// handleListMySessions lists the caller's live sessions, most recently seen
// first, marking the one this request came in on (GET /account/sessions). A
// caller signed in through Cloudflare Access has no session of its own, so none
// is marked.
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

// handleRevokeMySession signs out one of the caller's sessions
// (DELETE /account/sessions/{hash}). A hash that is not a live session of the
// caller is 404, whoever it belongs to. Revoking the session this request came
// in on is a sign-out, so the cookie is cleared too.
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

// handleRevokeMyOtherSessions signs out every session of the caller except the
// one this request came in on (POST /account/sessions/revoke-others), and says
// how many it ended.
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

// revokeOtherSessionsAfter signs out the caller's other devices after a change
// that retires a way in: a removed passkey, or a new verified email replacing the
// address sign-in codes went to. A session opened with the old factor ends with
// it. The change has already committed, so a failure here is logged and the
// request still succeeds; answering an error would invite retrying a change that
// took effect.
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

// callerSessionHash is the session the request authenticated with, or "" for a
// principal that did not come from a session cookie.
func callerSessionHash(r *http.Request, p *Principal) string {
	if p == nil || !p.ViaSession {
		return ""
	}
	return currentSessionHash(r)
}
