package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Setup-token redemption (spec §B setup bootstrap). The `felis setup` MC-bind
// flow mints a one-time token and prints a URL like:
//
//	https://op.console.<root>/setup?token=<raw>
//
// The Owner is staff, so onboarding lands on the operator console; the SPA there
// reads the token from the query
// string and POSTs it here. This handler consumes the token (single-use, hashed
// at rest like session cookies), mints a felis_session, and returns the caller's
// setup state so the frontend can guide email verification + passkey enrollment
// before unlocking the admin console.
//
// The minted session is a "lockdown" session in product terms: the Owner has not
// yet proven control of an email or enrolled a passkey, so the frontend restricts
// it to the setup wizard. Backend enforcement of the lockdown is a separate
// middleware concern (checking email_verified on the principal); this handler's
// job is the one-time token→session swap and reporting what setup remains.

// setupRedeemRequest is the redeem body: the raw one-time token from the setup URL.
type setupRedeemRequest struct {
	Token string `json:"token"`
}

// handleSetupRedeem consumes a one-time setup token and mints a lockdown session
// (Public, pre-session). The token is hashed (sha-256) before lookup — only the
// hash is persisted, mirroring session-cookie storage. On success the caller
// receives a felis_session cookie and a JSON body describing the remaining setup
// steps (email set? verified? passkey enrolled?) so the SPA can drive the wizard.
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

	// Hash the raw token — only the hash is stored (mirroring session cookies and
	// setup token creation in performSetupMCBind).
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])

	now := a.now()
	userID, err := a.Repo.ConsumeSetupToken(r.Context(), tokenHash, now)
	if err != nil {
		// Unknown, already-consumed, or expired — uniform 400 so the token cannot
		// be used as an oracle.
		a.authFailure(r, "setup_redeem", "bad_token", nil)
		writeError(w, r, newError(http.StatusBadRequest, "setup_token_invalid",
			"this setup link is invalid or has already been used"))
		return
	}

	u, err := a.Repo.UserByID(r.Context(), userID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Mint the session — a regular felis_session; the lockdown is a product-level
	// restriction the frontend enforces until email is verified / a passkey is bound.
	if err := a.startSession(w, r, u.ID); err != nil {
		writeError(w, r, err)
		return
	}

	// Report the setup state so the SPA knows which wizard steps remain.
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
		// setup_required MUST mirror requireOnboarded's unlock (api.go:615); see setupRequired.
		"setup_required": setupRequired(u.EmailVerified, hasPasskey),
	})
}

// handleSetupStatus reports the caller's setup progress (app-tier). The SPA polls
// it after each wizard step (email verify, passkey enroll) to decide whether the
// lockdown can lift. It reads only the principal's own state.
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
		// setup_required MUST mirror requireOnboarded's unlock (api.go:615); see setupRequired.
		"setup_required": setupRequired(u.EmailVerified, hasPasskey),
	})
}
