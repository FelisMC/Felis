package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Bind codes can create player sessions only when the internal caller minted them from an authenticated in-game UUID.

func newUserID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type bindRedeemRequest struct {
	Code string `json:"code"`
}

// handleBindRedeem permits returning-player login but never mints staff sessions.
func (a *API) handleBindRedeem(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body bindRedeemRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(body.Code))
	if code == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "code is required"))
		return
	}

	uid, err := newUserID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	userID, mcUUID, authSource, err := a.Repo.RedeemPlayerBindCode(r.Context(), uid, code, a.now())
	switch {
	case errors.Is(err, ErrLinkCodeInvalid):
		a.authFailure(r, "bind_redeem", "bad_code", nil)
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "bind code is invalid or expired"))
		return
	case errors.Is(err, ErrPlayerBindForbidden):
		a.authFailure(r, "bind_redeem", "staff_account", nil)
		writeError(w, r, newError(http.StatusForbidden, "staff_account",
			"that Minecraft account belongs to staff; sign in at the operator console"))
		return
	case errors.Is(err, ErrPlayerAccountRetired):
		a.authFailure(r, "bind_redeem", "account_retired", nil)
		// Disabled accounts cannot sign in; the code remains usable within its TTL if the account is re-enabled.
		writeError(w, r, newError(http.StatusForbidden, "account_retired",
			"this Minecraft account's Felis account is disabled or deleted; contact the operator"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}

	if err := a.startSession(w, r, userID, bindCodeSignIn); err != nil {
		writeError(w, r, err)
		return
	}
	a.auditEntry(r, AuditEntry{Actor: "mc:" + mcUUID, ActorUserID: userID, Action: "account.bind_redeem"})
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": userID, "linked": true, "mc_uuid": mcUUID, "auth_source": authSource,
	})
}
