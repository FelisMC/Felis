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
)

// Player email verification (spec §B2 onboarding). Forced web onboarding proves a
// player controls an email before it is bound to their account: they request a
// one-time code, the platform mails it, and they type it back. Only a matching,
// unexpired, unconsumed code flips users.email_verified true. The two halves are
// app-tier external routes — verifying your OWN email is an ordinary authenticated
// operation, scoped entirely to the principal (the body never names a user).
//
// The code is a short numeric secret, so two independent defenses bound brute
// force: a short TTL (otpTTL) and a per-code attempt cap (otpMaxAttempts) checked
// inside VerifyEmailOTP. Only the sha-256 of the code is ever stored; the digits
// live only in the email.

const (
	// otpTTL bounds how long a freshly mailed code is accepted. Long enough to
	// switch to an inbox and back, short enough that a leaked code is useless soon.
	otpTTL = 10 * time.Minute
	// otpMaxAttempts caps wrong guesses against one code before it locks (429). With
	// a 6-digit code (1e6 keyspace) five tries is a ~5e-6 chance of a blind hit; the
	// cap is enforced in VerifyEmailOTP (Repo), not here, so the fake and PG agree.
	otpMaxAttempts = 5
	// otpPurposeOnboard scopes a code to the onboarding email-proof flow. The column
	// exists so later flows (e.g. email change) can mint codes that never collide
	// with an onboarding code for the same user.
	otpPurposeOnboard = "onboard_email"
	// otpCodeDigits is the code length; otpCodeBound is its exclusive upper bound, so
	// a value in [0, otpCodeBound) zero-pads to exactly otpCodeDigits digits.
	otpCodeDigits = 6
	otpCodeBound  = 1_000_000
	// otpResendCooldown is the minimum spacing between OTP sends. Without it,
	// handleEmailOTPStart is an email-bomb primitive: an authenticated caller could
	// drive unbounded mail to any address they type. The cooldown is enforced on two
	// keys (principal and recipient) so neither one account fanning out across many
	// addresses, nor many accounts converging on one address, can flood a mailbox.
	otpResendCooldown = 60 * time.Second
)

// OTPMailer delivers a one-time code to an email address. It is a seam, not a
// dependency: the demo ships without SMTP, so a nil Mailer logs the code
// server-side instead of mailing it (a KNOWN-LIMITATION, never a code returned to
// the client). Production wires a real sender.
type OTPMailer interface {
	SendOTP(ctx context.Context, email, code string) error
}

// newEmailOTP returns a cryptographically random otpCodeDigits-digit numeric code.
// crypto/rand.Int over a 10^digits bound is uniform with no modulo bias; the value
// is zero-padded so every code is exactly otpCodeDigits long.
func newEmailOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(otpCodeBound))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", otpCodeDigits, n.Int64()), nil
}

// newOTPID returns an opaque random row id (128 bits, hex) for an email_otps row.
func newOTPID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// otpCodeHash maps a code to its storage key (sha-256 hex), reusing the session
// helper so the raw digits are never written to the database.
func otpCodeHash(code string) string { return hashCookie(code) }

// looksLikeEmail is a deliberately small sanity check, not RFC 5322: it rejects the
// obvious garbage (empty, no/multiple '@', '@' at an edge, whitespace, no dot in the
// domain) so a code is never minted against an un-mailable string. Real validation
// is delivery itself — a wrong-but-plausible address simply never yields a code.
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

// emailOTPStartRequest is the start-onboarding-verification body: the address the
// player wants to prove control of.
type emailOTPStartRequest struct {
	Email string `json:"email"`
}

// handleEmailOTPStart mints and delivers a one-time code for the caller's chosen
// email (spec §B2, external app face). The code is bound to the principal's user_id
// and the onboarding purpose; a re-request supersedes the prior code. The response
// NEVER carries the code — it is delivered out of band — only that it was sent and
// when it expires.
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
	// Atomically reserve the cooldown on both the caller and the recipient BEFORE
	// minting, so a burst of truly concurrent starts yields exactly one winner. Here
	// the throttle is the sole defense and each admitted send is a real, non-idempotent
	// email, so an allowed→record peek would let N goroutines slip past together and
	// bomb a mailbox. The recipient key is lower-cased so case variants of one address
	// can't sidestep the per-mailbox cap. If any later step fails the deferred rollback
	// frees both windows, so a failed mint or delivery never consumes the cooldown —
	// the same property the old record-after-send gave, now race-free.
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
	// The send succeeded: keep both reservations (the deferred rollback becomes a
	// no-op) so the cooldown windows stand.
	committed = true
	a.audit(r, auditActor(p), "account.email.otp_sent", "")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"sent":       true,
		"expires_at": expiresAt.UTC(),
	})
}

// emailOTPVerifyRequest is the verify body: the code the player read from the email.
type emailOTPVerifyRequest struct {
	Code string `json:"code"`
}

// handleEmailOTPVerify redeems a code for the caller (spec §B2, external app face).
// Outcomes mirror the link-verify shape: an invalid/expired/mismatched code → 400
// invalid_code, a locked code (too many wrong guesses) → 429 otp_locked, and on
// success the user's email is written and email_verified flips true. The verified
// address is echoed so the panel can render it.
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
	switch {
	case errors.Is(err, ErrOTPLocked):
		writeError(w, r, newError(http.StatusTooManyRequests, "otp_locked",
			"too many incorrect attempts; request a new code"))
		return
	case errors.Is(err, ErrOTPInvalid):
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "email code is invalid or expired"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	a.audit(r, auditActor(p), "account.email.verified", "")
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "email": email})
}

// deliverOTP hands the code to the configured Mailer, or — when none is wired (the
// demo) — logs it server-side as a KNOWN-LIMITATION. The code is logged ONLY in the
// no-mailer fallback and ONLY to the server log; it is never put in an HTTP response.
func (a *API) deliverOTP(ctx context.Context, email, code string) error {
	if a.Mailer == nil {
		log.Printf("email-otp: no Mailer configured; code for %s is %s (KNOWN-LIMITATION: demo has no SMTP)", email, code)
		return nil
	}
	return a.Mailer.SendOTP(ctx, email, code)
}

// setEmailRequest is the record-email body: the address to bind to the caller's
// account WITHOUT an OTP round-trip.
type setEmailRequest struct {
	Email string `json:"email"`
}

// handleSetEmail records the caller's email without verifying it (SetupAllowed). The
// setup bootstrap has no SMTP, so the Owner cannot receive an emailed code; the
// address is stored unverified and a later Settings/SMTP flow proves control of it.
// This is the setup wizard's Step-1 write. The OTP start/verify pair above is left
// intact for the Account page and for post-SMTP verification — this door deliberately
// does NOT touch email_verified.
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
	if err := a.Repo.SetUserEmail(r.Context(), p.UserID, email); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, auditActor(p), "account.email.set", "")
	writeJSON(w, http.StatusOK, map[string]any{"email": email})
}

// auditActor picks the most identifying actor string for a principal: the audited
// Access email when present, else the stable user id. A player mid-onboarding may
// not have a verified email yet, so the id keeps the audit row attributable.
func auditActor(p *Principal) string {
	if p.Email != "" {
		return p.Email
	}
	return p.UserID
}
