package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Setup-token sessions remain restricted by onboarding middleware until the account meets setup requirements.

type setupRedeemRequest struct {
	Token string `json:"token"`
}

func (a *API) handleSetupRedeem(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req setupRedeemRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "token is required"))
		return
	}

	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])

	// Consume the setup token and persist the session atomically so a storage failure does not invalidate the setup link.
	sessionToken, session, err := a.mintSession(r, "", provenSignIn)
	if err != nil {
		writeError(w, r, err)
		return
	}
	userID, err := a.Repo.RedeemSetupToken(r.Context(), tokenHash, a.now(), session)
	switch {
	case errors.Is(err, ErrNotFound):
		// Return the same 400 for unknown, consumed and expired tokens.
		a.authFailure(r, "setup_redeem", "bad_token", nil)
		writeError(w, r, newError(http.StatusBadRequest, "setup_token_invalid",
			"this setup link is invalid or has already been used"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	setSessionCookie(w, sessionToken, session.ExpiresAt)

	u, err := a.Repo.UserByID(r.Context(), userID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	creds, _ := a.Repo.PasskeyCredentialsForUser(r.Context(), u.ID)
	hasPasskey := len(creds) > 0

	a.auditAccount(r, u, "auth.setup_redeem", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":        u.ID,
		"username":       u.Username,
		"role":           u.Role,
		"email":          u.Email,
		"email_verified": u.EmailVerified,
		"has_passkey":    hasPasskey,
		// Keep setup_required consistent with requireOnboarded via setupRequired.
		"setup_required": setupRequired(u.EmailVerified, hasPasskey),
	})
}

func (a *API) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	u, err := a.Repo.UserByID(r.Context(), p.UserID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "user not found"))
			return
		}
		writeError(w, r, err)
		return
	}
	creds, _ := a.Repo.PasskeyCredentialsForUser(r.Context(), u.ID)
	hasPasskey := len(creds) > 0
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":        u.ID,
		"username":       u.Username,
		"role":           u.Role,
		"email":          u.Email,
		"email_verified": u.EmailVerified,
		"has_passkey":    hasPasskey,
		// Keep setup_required consistent with requireOnboarded via setupRequired.
		"setup_required": setupRequired(u.EmailVerified, hasPasskey),
	})
}
