package api

import (
	"errors"
	"net/http"
	"strings"
)

// otpPurposeLogin isolates login codes from onboarding codes.
const otpPurposeLogin = "login_email"

type loginEmailStartRequest struct {
	Email string `json:"email"`
}

// handleLoginEmailStart preserves existing codes and returns the same 202 for unknown, locked and verified accounts.
func (a *API) handleLoginEmailStart(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req loginEmailStartRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	if !looksLikeEmail(email) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "a valid email is required"))
		return
	}

	// Check the mail budget before resolving the address so every address receives the same 429.
	if err := a.checkMailBudget(r.Context()); err != nil {
		writeError(w, r, err)
		return
	}

	// Reserve atomically before sending; namespace the recipient key separately from onboarding.
	emailKey := "login:email:" + strings.ToLower(email)
	lim := a.otpLimiter()
	emailAt, ok := lim.reserve(emailKey, otpResendCooldown)
	if !ok {
		// Reuse the existing code and expiry during the cooldown without sending another email.
		writeJSON(w, http.StatusAccepted, map[string]any{"sent": true, "expires_at": emailAt.Add(otpTTL).UTC()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			lim.release(emailKey, emailAt)
		}
	}()

	// Use the reservation's expiry so all accepted responses have identical bodies.
	expiresAt := emailAt.Add(otpTTL)

	u, err := a.Repo.UserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, ErrNotFound):
		// Keep the cooldown for unknown addresses to prevent enumeration through resend behavior.
		committed = true
		writeJSON(w, http.StatusAccepted, map[string]any{"sent": true, "expires_at": expiresAt.UTC()})
		return
	case err != nil:
		// Release the cooldown on store failure so an outage does not consume it.
		writeError(w, r, err)
		return
	}

	// Locked accounts receive the same 202 without email to prevent enumeration.
	switch until, err := a.Repo.OTPLockedUntil(r.Context(), u.ID, otpPurposeLogin, a.now()); {
	case err != nil:
		writeError(w, r, err)
		return
	case !until.IsZero():
		committed = true
		writeJSON(w, http.StatusAccepted, map[string]any{"sent": true, "expires_at": expiresAt.UTC()})
		return
	}

	code, err := newEmailOTP()
	if err != nil {
		writeError(w, r, err)
		return
	}
	id, err := newOTPID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Deliver to the stored address; lookup is case-insensitive.
	if err := a.Repo.AddLoginEmailOTP(r.Context(), id, u.ID, u.Email, otpCodeHash(code), otpPurposeLogin, a.now(), expiresAt); err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.deliverOTP(r.Context(), u.Email, code); err != nil {
		writeError(w, r, err)
		return
	}
	committed = true
	a.auditAccount(r, u, "auth.login_email.otp_sent", "")
	writeJSON(w, http.StatusAccepted, map[string]any{"sent": true, "expires_at": expiresAt.UTC()})
}

type loginEmailVerifyRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// handleLoginEmailVerify returns a uniform invalid_code response and refuses staff only after consuming valid proof.
func (a *API) handleLoginEmailVerify(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req loginEmailVerifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	code := strings.TrimSpace(req.Code)
	if !looksLikeEmail(email) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "a valid email is required"))
		return
	}
	if code == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "code is required"))
		return
	}

	u, err := a.Repo.UserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, ErrNotFound):
		// Unknown accounts and wrong codes must return the same response.
		a.authFailure(r, "login_email", "no_account", nil)
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "email code is invalid or expired"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}

	// Consume before checking the role to prevent staff enumeration; login proof must not rewrite the account email.
	switch err := a.Repo.ConsumeLoginEmailOTP(r.Context(), u.ID, otpPurposeLogin, otpCodeHash(code), a.now()); {
	case errors.Is(err, ErrOTPInvalid), errors.Is(err, ErrOTPLocked), errors.Is(err, ErrOTPAccountLocked):
		a.noteOTPLock(r, err, u.ID, otpPurposeLogin)
		a.authFailure(r, "login_email", otpFailureReason(err), u)
		// Use the same 400 for locked, expired and wrong codes to prevent account enumeration.
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "email code is invalid or expired"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}

	// Refuse staff after consuming proof so player login cannot bypass staff authentication.
	if u.Role != "user" {
		a.authFailure(r, "login_email", "staff_account", u)
		writeError(w, r, newError(http.StatusForbidden, "staff_account",
			"that account is staff; sign in at the operator console"))
		return
	}

	if err := a.startSession(w, r, u.ID, provenSignIn); err != nil {
		writeError(w, r, err)
		return
	}
	a.auditAccount(r, u, "auth.login_email", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": u.ID,
		"role":    u.Role,
	})
}
