package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// Account-factor changes require recent proof; bind codes and staff email codes alone do not qualify.

const (
	reauthWindow = 5 * time.Minute

	otpPurposeReauth     = "reauth"
	passkeyPurposeReauth = "passkey_reauth"

	reauthFactorPasskey = "passkey"
	reauthFactorEmail   = "email"
	reauthFactorSignIn  = "sign_in"
)

type reauthState struct {
	Needed bool `json:"needed"`
	// Until is absent without a current proof or an account factor to protect.
	Until *time.Time `json:"until,omitempty"`
	// Factors lists supported reauth methods in preference order.
	Factors []string `json:"factors"`
}

func (a *API) reauthState(r *http.Request, p *Principal) (reauthState, error) {
	st := reauthState{Factors: []string{}}
	if !p.ViaSession {
		// Non-session callers have no session reauth state to update.
		return st, nil
	}
	hasPasskey, err := a.userHasPasskey(r.Context(), p.UserID)
	if err != nil {
		return st, err
	}
	if hasPasskey {
		st.Factors = append(st.Factors, reauthFactorPasskey)
	}
	// Email is a usable account factor only while a mail relay is available.
	emailWayIn := p.EmailVerified && a.mailer(r.Context()) != nil
	if emailWayIn {
		if staffRole(p.Role) {
			st.Factors = append(st.Factors, reauthFactorSignIn)
		} else {
			st.Factors = append(st.Factors, reauthFactorEmail)
		}
	}
	if !hasPasskey && !emailWayIn {
		// The session is the account's only sign-in method, so there is no other factor to protect.
		return st, nil
	}
	if until := p.ReauthAt.Add(reauthWindow); !p.ReauthAt.IsZero() && a.now().Before(until) {
		until = until.UTC()
		st.Until = &until
		return st, nil
	}
	st.Needed = true
	return st, nil
}

// requireReauth writes the refusal response when returning false.
func (a *API) requireReauth(w http.ResponseWriter, r *http.Request, p *Principal) bool {
	if p != nil && p.APIKey != "" {
		writeError(w, r, newError(http.StatusForbidden, "session_required",
			"this change needs a signed-in session; an API key cannot confirm it's you"))
		return false
	}
	st, err := a.reauthState(r, p)
	if err != nil {
		writeError(w, r, err)
		return false
	}
	if st.Needed {
		writeError(w, r, newError(http.StatusForbidden, "reauth_required",
			"confirm it's you first: this change needs your passkey or email code from the last few minutes"))
		return false
	}
	return true
}

func (a *API) markReauth(w http.ResponseWriter, r *http.Request, p *Principal, factor string) {
	now := a.now()
	if err := a.Repo.MarkSessionReauth(r.Context(), currentSessionHash(r), now); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.reauth", factor)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "until": now.Add(reauthWindow).UTC()})
}

// markReauthQuietly records proof after a completed change; failure leaves the next guarded change requiring reauth.
func (a *API) markReauthQuietly(r *http.Request) {
	hash := currentSessionHash(r)
	if hash == "" {
		return
	}
	if err := a.Repo.MarkSessionReauth(r.Context(), hash, a.now()); err != nil {
		log.Printf("auth: could not record reauth on the session (request_id=%s): %v",
			requestIDFromContext(r.Context()), err)
	}
}

func (a *API) handleReauthStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.reauthState(r, principalFromContext(r.Context()))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func requireReauthSession(w http.ResponseWriter, r *http.Request, p *Principal) bool {
	if !p.ViaSession || currentSessionHash(r) == "" {
		writeError(w, r, newError(http.StatusBadRequest, "no_session",
			"only a signed-in browser session can confirm it's you"))
		return false
	}
	return true
}

func (a *API) handleReauthPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	if !requireReauthSession(w, r, p) {
		return
	}
	a.beginStepUpPasskey(w, r, p, passkeyPurposeReauth,
		"no passkey enrolled; confirm with an email code instead")
}

func (a *API) handleReauthPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	var req stepUpPasskeyFinishRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if len(req.Assertion) == 0 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "assertion is required"))
		return
	}
	if !requireReauthSession(w, r, p) {
		return
	}
	if !a.finishStepUpPasskey(w, r, p, passkeyPurposeReauth, "reauth_passkey", req.Assertion) {
		return
	}
	a.markReauth(w, r, p, reauthFactorPasskey)
}

// requireEmailReauth excludes staff, who must use a passkey or repeat op-login.
func requireEmailReauth(w http.ResponseWriter, r *http.Request, p *Principal) bool {
	if staffRole(p.Role) {
		writeError(w, r, newError(http.StatusForbidden, "staff_reauth",
			"operators confirm with a passkey or by signing in again"))
		return false
	}
	if !p.EmailVerified || p.Email == "" {
		writeError(w, r, newError(http.StatusConflict, "no_step_up_factor",
			"there is no verified email on this account to send a code to"))
		return false
	}
	return true
}

func (a *API) handleReauthEmailStart(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !requireReauthSession(w, r, p) || !requireEmailReauth(w, r, p) {
		return
	}
	a.startStepUpOTP(w, r, p, otpPurposeReauth, "reauth:", "account.reauth.otp_sent")
}

func (a *API) handleReauthEmailVerify(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	var req stepUpOTPVerifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "code is required"))
		return
	}
	if !requireReauthSession(w, r, p) || !requireEmailReauth(w, r, p) {
		return
	}
	if !a.verifyStepUpOTP(w, r, p, otpPurposeReauth, "reauth_email", code) {
		return
	}
	a.markReauth(w, r, p, reauthFactorEmail)
}

type stepUpPasskeyFinishRequest struct {
	Assertion json.RawMessage `json:"assertion"`
}

type stepUpOTPVerifyRequest struct {
	Code string `json:"code"`
}

// stepUpPasskeyUser must use the same credential set at begin and finish.
func stepUpPasskeyUser(p *Principal, creds []PasskeyCredential) PasskeyUser {
	name := p.Email
	if name == "" {
		name = p.UserID
	}
	return PasskeyUser{ID: p.UserID, Name: name, DisplayName: name, Credentials: creds}
}

// beginStepUpPasskey requires a configured Passkey verifier.
func (a *API) beginStepUpPasskey(w http.ResponseWriter, r *http.Request, p *Principal, purpose, noPasskey string) {
	creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if len(creds) == 0 {
		writeError(w, r, newError(http.StatusBadRequest, "no_passkey", "%s", noPasskey))
		return
	}
	options, sessionData, err := a.Passkey.BeginLogin(stepUpPasskeyUser(p, creds))
	if err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_failed",
			"could not start passkey confirmation"))
		return
	}
	id, err := newPasskeyID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	expiresAt := a.now().Add(passkeyChallengeTTL)
	if err := a.Repo.CreatePasskeyChallenge(r.Context(), id, p.UserID, purpose, sessionData, expiresAt); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, options)
}

// finishStepUpPasskey requires a verifier and assertion; false means the error response was written.
func (a *API) finishStepUpPasskey(w http.ResponseWriter, r *http.Request, p *Principal, purpose, door string, assertion json.RawMessage) bool {
	invalid := func() bool {
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
			"passkey confirmation could not be completed; begin again"))
		return false
	}
	sessionData, err := a.Repo.ConsumePasskeyChallengeByUser(r.Context(), p.UserID, purpose, a.now())
	if err != nil {
		if errors.Is(err, ErrPasskeyChallengeInvalid) {
			a.authFailure(r, door, "challenge_invalid", nil)
			return invalid()
		}
		writeError(w, r, err)
		return false
	}
	creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return false
	}
	va, err := a.Passkey.FinishLogin(stepUpPasskeyUser(p, creds), sessionData, bytes.NewReader(assertion))
	if err != nil {
		a.authFailure(r, door, "bad_assertion", nil)
		return invalid()
	}
	// Apply the login policy for user verification, clone detection and sign-count updates.
	if err := a.applyAssertion(r.Context(), va, creds); err != nil {
		if a.passkeyAssertionRejected(r, door, nil, va.CredentialID, err) {
			return invalid()
		}
		writeError(w, r, err)
		return false
	}
	return true
}

// startStepUpOTP namespaces resend cooldowns by keyPrefix so ceremonies do not throttle each other.
func (a *API) startStepUpOTP(w http.ResponseWriter, r *http.Request, p *Principal, purpose, keyPrefix, auditAction string) {
	if err := a.checkMailBudget(r.Context()); err != nil {
		writeError(w, r, err)
		return
	}
	if until, err := a.Repo.OTPLockedUntil(r.Context(), p.UserID, purpose, a.now()); err != nil {
		writeError(w, r, err)
		return
	} else if !until.IsZero() {
		writeOTPAccountLocked(w, r, until, a.now())
		return
	}
	emailKey := keyPrefix + strings.ToLower(p.Email)
	lim := a.otpLimiter()
	emailAt, ok := lim.reserve(emailKey, otpResendCooldown)
	if !ok {
		writeError(w, r, newError(http.StatusTooManyRequests, "otp_resend_cooldown",
			"a code was sent recently; wait a moment before requesting another"))
		return
	}
	committed := false
	defer func() {
		if !committed {
			lim.release(emailKey, emailAt)
		}
	}()
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
	expiresAt := a.now().Add(otpTTL)
	if err := a.Repo.CreateEmailOTP(r.Context(), id, p.UserID, p.Email, otpCodeHash(code), purpose, expiresAt); err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.deliverOTP(r.Context(), p.Email, code); err != nil {
		writeError(w, r, err)
		return
	}
	committed = true
	a.audit(r, auditAction, "")
	writeJSON(w, http.StatusAccepted, map[string]any{"sent": true, "expires_at": expiresAt.UTC()})
}

// verifyStepUpOTP consumes proof without changing the account address; false means the error response was written.
func (a *API) verifyStepUpOTP(w http.ResponseWriter, r *http.Request, p *Principal, purpose, door, code string) bool {
	var lock *OTPAccountLockedError
	err := a.Repo.ConsumeLoginEmailOTP(r.Context(), p.UserID, purpose, otpCodeHash(code), a.now())
	if isOTPRefusal(err) {
		a.authFailure(r, door, otpFailureReason(err), nil)
	}
	switch {
	case errors.As(err, &lock):
		a.noteOTPLock(r, err, p.UserID, purpose)
		writeOTPAccountLocked(w, r, lock.Until, a.now())
		return false
	case errors.Is(err, ErrOTPLocked):
		writeError(w, r, newError(http.StatusTooManyRequests, "otp_locked",
			"too many incorrect attempts; request a new code"))
		return false
	case errors.Is(err, ErrOTPInvalid):
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "email code is invalid or expired"))
		return false
	case err != nil:
		writeError(w, r, err)
		return false
	}
	return true
}
