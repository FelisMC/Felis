package api

import (
	"errors"
	"net/http"
	"strings"
)

// Auth options intentionally reveal available login methods, but compute them identically for staff and players.
type authOptionsRequest struct {
	Email string `json:"email"`
}

func (a *API) handleAuthOptions(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req authOptionsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	if !looksLikeEmail(email) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "a valid email is required"))
		return
	}

	methods := []string{}

	u, err := a.Repo.UserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusOK, map[string]any{"methods": methods})
		return
	case err != nil:
		writeError(w, r, err)
		return
	}

	// Do not branch on role or advertise passkeys without a configured verifier.
	if a.Passkey != nil {
		creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), u.ID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if len(creds) > 0 {
			methods = append(methods, "passkey")
		}
	}
	if a.mailer(r.Context()) != nil {
		methods = append(methods, "email_otp")
	}

	writeJSON(w, http.StatusOK, map[string]any{"methods": methods})
}
