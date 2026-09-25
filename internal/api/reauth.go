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

// Reauth (step-up) guards the changes that plant or remove a lasting way into an
// account: adding or removing a passkey and changing the email. Holding the
// session is not enough for them once the account has a factor of its own; the
// holder must have proven one within reauthWindow. Otherwise a stolen cookie
// (XSS, a shared machine) could register the thief's passkey and keep the
// account long after the session ends, and for staff that passkey would skip
// op-login's in-game approval for good.
//
// What proves a factor, and so marks the session (sessions.reauth_at):
//
//   - signing in by passkey, by email code, through op-login or with the setup
//     token (startSession with provenSignIn);
//   - a passkey assertion or an email code on the reauth endpoints below;
//   - verifying an email address by code (the address is proven that moment,
//     and reaching that step already passed this gate when the account had a
//     factor to protect).
//
// A bind-code sign-in proves only the in-game identity and marks nothing: whoever
// controls the Minecraft account must still show the account's passkey or mailbox
// before touching them.
//
// Staff reauth with a passkey or by signing in again through op-login. An email
// code alone is not a staff factor, because signing in as staff by email also
// takes in-game approval.

const (
	// reauthWindow is how long a proven factor lets the session make guarded
	// changes. Long enough to finish a passkey ceremony or an email change.
	reauthWindow = 5 * time.Minute

	otpPurposeReauth     = "reauth"
	passkeyPurposeReauth = "passkey_reauth"

	reauthFactorPasskey = "passkey"
	reauthFactorEmail   = "email"
	// reauthFactorSignIn: sign out and back in through a proving door.
	reauthFactorSignIn = "sign_in"
)

// reauthState is where the caller stands with the guarded changes.
type reauthState struct {
	// Needed: a guarded change would be refused until the caller reauths.
	Needed bool `json:"needed"`
	// Until is when the current proof stops counting; absent when there is none
	// or the account has nothing to guard.
	Until *time.Time `json:"until,omitempty"`
	// Factors are the ways this caller can reauth, best first.
	Factors []string `json:"factors"`
}

func (a *API) reauthState(r *http.Request, p *Principal) (reauthState, error) {
	st := reauthState{Factors: []string{}}
	if !p.ViaSession {
		// A Cloudflare Access caller is authenticated by the proxy on every
		// request and has no session here to mark.
		return st, nil
	}
	hasPasskey, err := a.userHasPasskey(r.Context(), p.UserID)
	if err != nil {
		return st, err
	}
	if hasPasskey {
		st.Factors = append(st.Factors, reauthFactorPasskey)
	}
	// A verified email is a way in only while a relay can mail it a code: with
	// none, the email and op-login doors answer 503 mail_unavailable, so it is
	// neither a factor to offer nor a door to guard.
	emailWayIn := p.EmailVerified && a.Mailer != nil
	if emailWayIn {
		if staffRole(p.Role) {
			st.Factors = append(st.Factors, reauthFactorSignIn)
		} else {
			st.Factors = append(st.Factors, reauthFactorEmail)
		}
	}
	if !hasPasskey && !emailWayIn {
		// Nothing to protect yet: the session is the account's only way in.
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

// requireReauth lets a guarded change through, or answers 403 reauth_required
// and returns false.
func (a *API) requireReauth(w http.ResponseWriter, r *http.Request, p *Principal) bool {
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

// markReauth records the proof on the caller's session and answers with the new
// window.
func (a *API) markReauth(w http.ResponseWriter, r *http.Request, p *Principal, factor string) {
	now := a.now()
	if err := a.Repo.MarkSessionReauth(r.Context(), currentSessionHash(r), now); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.reauth", factor)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "until": now.Add(reauthWindow).UTC()})
}

// markReauthQuietly records a proof that happened as part of another change
// (a passkey registration, an email verification). The change already went
// through, so a failure here is logged and the next guarded change just asks.
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

// handleReauthStatus reports whether a guarded change needs a reauth first and
// which factors can provide it, so the panel can ask before starting one.
func (a *API) handleReauthStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.reauthState(r, principalFromContext(r.Context()))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// requireReauthSession refuses the reauth endpoints to a caller with no session
// to mark.
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

// requireEmailReauth admits a player with a verified address to the email-code
// reauth; staff confirm with a passkey or by signing in again.
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

// ---- step-up ceremonies shared by reauth and the migration confirm ----

// stepUpPasskeyFinishRequest is the assertion the browser produced, captured as
// raw bytes so the exact response reaches the verifier without re-encoding.
type stepUpPasskeyFinishRequest struct {
	Assertion json.RawMessage `json:"assertion"`
}

// stepUpOTPVerifyRequest is the code from the step-up email.
type stepUpOTPVerifyRequest struct {
	Code string `json:"code"`
}

// stepUpPasskeyUser builds the PasskeyUser the assertion ceremony needs for the
// already signed-in caller (contrast the login door, which resolves it from a
// typed email). The credential set must be identical between begin and finish.
func stepUpPasskeyUser(p *Principal, creds []PasskeyCredential) PasskeyUser {
	name := p.Email
	if name == "" {
		name = p.UserID
	}
	return PasskeyUser{ID: p.UserID, Name: name, DisplayName: name, Credentials: creds}
}

// beginStepUpPasskey starts an assertion over the caller's own passkeys, its
// challenge stashed under purpose, and writes the options (go-webauthn's
// {"publicKey": {...}} document). The caller has checked a.Passkey.
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

// finishStepUpPasskey consumes the purpose's stashed challenge and verifies the
// assertion against the caller's passkeys. It reports whether the caller passed;
// on false the error is written. door names the failure in metrics and audit.
// The caller has checked a.Passkey and that the assertion is present.
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
	// Same UV and clone policy as the login door (applyAssertion): an unverified
	// user or a rolled-back counter fails closed with the opaque envelope, so a
	// step-up never accepts an authenticator that login refuses. A clean assertion
	// advances the stored sign-count, keeping the clone signal meaningful.
	if err := a.applyAssertion(r.Context(), va, creds); err != nil {
		if a.passkeyAssertionRejected(r, door, nil, va.CredentialID, err) {
			return invalid()
		}
		writeError(w, r, err)
		return false
	}
	return true
}

// startStepUpOTP mails a fresh code under purpose to the caller's (verified)
// address and answers 202. keyPrefix namespaces the per-mailbox resend cooldown
// so the step-up doors never perturb each other's throttle.
func (a *API) startStepUpOTP(w http.ResponseWriter, r *http.Request, p *Principal, purpose, keyPrefix, auditAction string) {
	if err := a.checkMailBudget(); err != nil {
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

// verifyStepUpOTP redeems a step-up code. The lifecycle is the login door's (no
// identity side effect): the address is already proven. It reports whether the
// code matched; on false the error is written.
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
