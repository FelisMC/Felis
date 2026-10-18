package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"felis.lolicon.best/internal/metrics"
)

const (
	otpTTL = 10 * time.Minute
	// otpMaxAttempts bounds guesses per code; VerifyEmailOTP enforces it atomically.
	otpMaxAttempts = 5
	// otpPurposeOnboard isolates address-verification codes from other OTP flows.
	otpPurposeOnboard = "onboard_email"
	otpCodeDigits     = 6
	otpCodeBound      = 1_000_000
	// otpResendCooldown applies per principal and recipient to bound mailbox flooding.
	otpResendCooldown = 60 * time.Second
	// otpFailureBudget applies per (user, purpose) across resends, which reset the per-code attempt count.
	otpFailureBudget = 10
	otpFailureWindow = 24 * time.Hour
	// otpLiveLoginCodes preserves recent codes so unauthenticated starts cannot invalidate a recipient's sign-in proof.
	otpLiveLoginCodes = 3
)

// OTPMailer is nil without a relay; OTP endpoints then return mail_unavailable before minting codes.
type OTPMailer interface {
	SendOTP(ctx context.Context, email, code string) error
}

// newEmailOTP uses uniform cryptographic sampling and zero-pads to otpCodeDigits.
func newEmailOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(otpCodeBound))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", otpCodeDigits, n.Int64()), nil
}

func newOTPID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func otpCodeHash(code string) string { return hashCookie(code) }

// looksLikeEmail checks basic syntax; delivery proves that the address is usable.
func looksLikeEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at != strings.LastIndexByte(s, '@') || at == len(s)-1 {
		return false
	}
	domain := s[at+1:]
	dot := strings.IndexByte(domain, '.')
	return dot > 0 && dot < len(domain)-1
}

type emailOTPStartRequest struct {
	Email string `json:"email"`
}

// handleEmailOTPStart supersedes the previous onboarding code and delivers it only by email.
func (a *API) handleEmailOTPStart(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	var req emailOTPStartRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	if !looksLikeEmail(email) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "a valid email is required"))
		return
	}
	// Check mail availability before requiring proof that depends on it.
	if err := a.checkMailBudget(r.Context()); err != nil {
		writeError(w, r, err)
		return
	}
	if !a.requireReauth(w, r, p) {
		return
	}
	// Reserve principal and recipient cooldowns atomically; failures release both, and locked accounts receive no new code.
	if until, err := a.Repo.OTPLockedUntil(r.Context(), p.UserID, otpPurposeOnboard, a.now()); err != nil {
		writeError(w, r, err)
		return
	} else if !until.IsZero() {
		writeOTPAccountLocked(w, r, until, a.now())
		return
	}
	userKey, emailKey := "user:"+p.UserID, "email:"+strings.ToLower(email)
	lim := a.otpLimiter()
	userAt, ok := lim.reserve(userKey, otpResendCooldown)
	if !ok {
		writeError(w, r, newError(http.StatusTooManyRequests, "otp_resend_cooldown",
			"a code was sent recently; wait a moment before requesting another"))
		return
	}
	emailAt, ok := lim.reserve(emailKey, otpResendCooldown)
	if !ok {
		lim.release(userKey, userAt)
		writeError(w, r, newError(http.StatusTooManyRequests, "otp_resend_cooldown",
			"a code was sent recently; wait a moment before requesting another"))
		return
	}
	committed := false
	defer func() {
		if !committed {
			lim.release(userKey, userAt)
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
	if err := a.Repo.CreateEmailOTP(r.Context(), id, p.UserID, email, otpCodeHash(code), otpPurposeOnboard, expiresAt); err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.deliverOTP(r.Context(), email, code); err != nil {
		writeError(w, r, err)
		return
	}
	committed = true
	a.audit(r, "account.email.otp_sent", "")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"sent":       true,
		"expires_at": expiresAt.UTC(),
	})
}

type emailOTPVerifyRequest struct {
	Code string `json:"code"`
}

// handleEmailOTPVerify leaves the code live on email_taken so the caller can resolve the address conflict.
func (a *API) handleEmailOTPVerify(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	var req emailOTPVerifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "code is required"))
		return
	}
	email, err := a.Repo.VerifyEmailOTP(r.Context(), p.UserID, otpPurposeOnboard, otpCodeHash(code), a.now())
	if isOTPRefusal(err) {
		a.authFailure(r, "onboard_email", otpFailureReason(err), nil)
	}
	var lock *OTPAccountLockedError
	switch {
	case errors.As(err, &lock):
		a.noteOTPLock(r, err, p.UserID, otpPurposeOnboard)
		writeOTPAccountLocked(w, r, lock.Until, a.now())
		return
	case errors.Is(err, ErrOTPLocked):
		writeError(w, r, newError(http.StatusTooManyRequests, "otp_locked",
			"too many incorrect attempts; request a new code"))
		return
	case errors.Is(err, ErrOTPInvalid):
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "email code is invalid or expired"))
		return
	case errors.Is(err, ErrEmailTaken):
		writeError(w, r, newError(http.StatusConflict, "email_taken",
			"that email is already verified on another account; sign in with it or use another address"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.email.verified", "")
	a.markReauthQuietly(r)
	// Changing a verified address retires sessions opened through the old address and notifies that mailbox.
	if p.EmailVerified && !strings.EqualFold(p.Email, email) {
		a.revokeOtherSessionsAfter(r, "email change")
		a.notifyEmailChanged(r, p.Email, email)
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "email": email})
}

// deliverOTP spends the shared mail budget; codes must never appear in responses or logs.
func (a *API) deliverOTP(ctx context.Context, email, code string) error {
	mailer := a.mailer(ctx)
	if mailer == nil {
		return errMailUnavailable()
	}
	if ok, wait := a.mailGate().take(mailGateKey); !ok {
		metrics.MailTotal.WithLabelValues("otp", "throttled").Inc()
		log.Printf("api: OTP mail refused by the install-wide mail budget (request_id=%s)", requestIDFromContext(ctx))
		return errMailRateLimited(wait)
	}
	if err := mailer.SendOTP(ctx, email, code); err != nil {
		metrics.MailTotal.WithLabelValues("otp", "failed").Inc()
		// Keep relay diagnostics in logs because they may expose SMTP account details.
		log.Printf("api: OTP delivery failed (request_id=%s): %v", requestIDFromContext(ctx), err)
		return newError(http.StatusBadGateway, "mail_undeliverable",
			"the mail relay refused this message; ask the server operator to check the SMTP settings")
	}
	metrics.MailTotal.WithLabelValues("otp", "sent").Inc()
	return nil
}

type setEmailRequest struct {
	Email string `json:"email"`
}

// handleSetEmail records an unverified address for setup before SMTP is configured.
func (a *API) handleSetEmail(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req setEmailRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	if !looksLikeEmail(email) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "a valid email is required"))
		return
	}
	// Changing the address removes the current email factor, so it requires reauth.
	if !a.requireReauth(w, r, p) {
		return
	}
	if err := a.Repo.SetUserEmail(r.Context(), p.UserID, email); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.email.set", "")
	writeJSON(w, http.StatusOK, map[string]any{"email": email})
}
