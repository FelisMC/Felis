package api

import (
	"errors"
	"net/http"
	"strings"
)

// Pre-session Email-OTP LOGIN (spec §B, console.<root_domain> returning-player door).
// This is the returning-player counterpart of handleBindRedeem: an account that
// already proved control of an email (email_verified, migration 0010) logs back in
// with a one-time code mailed to that address — no password exists anywhere in the
// product, and no in-game Bind Code is needed the second time. The two halves are
// Public, pre-session routes: the caller has no principal yet, so identity is resolved from
// the typed email via UserByEmail, exactly as handleBindRedeem resolves it from the
// code.
//
// Distinct from the authenticated /account/email/* onboarding pair in three ways,
// all load-bearing:
//
//   - Purpose. Codes are minted under otpPurposeLogin ("login_email"), never
//     otpPurposeOnboard, so a login code and an onboarding code for the same user
//     never clobber or satisfy each other (the email_otps purpose column is exactly
//     this separator).
//   - No principal. The throttle cannot key off a user id (there is none yet); it
//     keys off the typed recipient address, the same anti-bomb dimension the onboard
//     start uses. Volume from one client is bounded separately by the per-address
//     token bucket every public auth door sits behind (throttleAuthDoor), and total
//     mail by the install-wide mail budget (ratelimit.go).
//   - Refuse staff. Like handleBindRedeem this public door provably never mints a
//     session for an admin identity: op.console stays behind Zero Trust (and its own
//     in-game approval gate). The refusal happens only AFTER a valid code is
//     redeemed (see handleLoginEmailVerify), so a caller without the code cannot use
//     it to enumerate which addresses are staff.
//
// Enumeration is an accepted product decision (a dedicated /auth/options oracle is a
// sibling slice), so this pair does not go out of its way to equalise timing between
// existing and unknown addresses — it only keeps the *verify* response uniform so a
// code-less caller learns nothing a wrong guess would not already reveal.

// otpPurposeLogin scopes a code to the pre-session email LOGIN flow, keeping it from
// ever colliding with or satisfying an onboarding-email code (otpPurposeOnboard) for
// the same user. VerifyEmailOTP is queried per (user, purpose), so the two flows are
// fully independent even for one account with both a live onboarding and a live
// login code.
const otpPurposeLogin = "login_email"

// loginEmailStartRequest is the start-login-by-email body: the address whose mailbox
// the returning player will read the code from.
type loginEmailStartRequest struct {
	Email string `json:"email"`
}

// handleLoginEmailStart mints and mails a login code for a returning account (Public,
// pre-session). It gates on local sessions being enabled — like handleBindRedeem
// and the op-login door, minting a code toward a felis_session while SessionAuth
// would reject that cookie is pointless — reserves the per-recipient cooldown, resolves the
// address to an account, and (only if one exists) mints a code under otpPurposeLogin.
// An address with no verified account yields the SAME 202 as a successful send with
// no code minted: the response never distinguishes the two, and the reservation is
// kept on that path too so repeated probing of one address is throttled identically
// to repeated sends.
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

	// The install-wide mail budget is checked before the address is resolved,
	// so while it is spent every address gets the same 429.
	if err := a.checkMailBudget(); err != nil {
		writeError(w, r, err)
		return
	}

	// Atomically reserve the per-recipient cooldown BEFORE any work, so a burst of
	// truly concurrent starts yields exactly one winner and each admitted send is one
	// real, non-idempotent email. The key is namespaced apart from the onboard door's
	// "email:" key on purpose: this door is unauthenticated, so it must not perturb
	// the authenticated onboarding throttle. Both caps are 1/window, so a mailbox sees
	// at most one login code plus one onboard code per window — far below any bomb.
	emailKey := "login:email:" + strings.ToLower(email)
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

	// Compute the expiry once so the neutral (no-account) branch and the real-send
	// branch return byte-identical bodies.
	expiresAt := a.now().Add(otpTTL)

	u, err := a.Repo.UserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, ErrNotFound):
		// No verified account for this address. Return the same 202 as a real send
		// (no code minted) and KEEP the reservation, so probing an unknown address is
		// throttled exactly like resending to a known one — the throttle reveals
		// nothing, and the accepted /auth/options oracle is where existence is learnt.
		committed = true
		writeJSON(w, http.StatusAccepted, map[string]any{"sent": true, "expires_at": expiresAt.UTC()})
		return
	case err != nil:
		// A real read error is NOT a neutral outcome: leave committed false so the
		// deferred rollback frees the window (a transient DB blip must not burn it).
		writeError(w, r, err)
		return
	}

	// A locked door (wrong-code budget spent) gets the same neutral 202 and no
	// mail: the owner was told by the lock notice, and a distinct answer here
	// would tell a prober the address has an account.
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
	// Mint and deliver against the account's STORED address, not the typed string:
	// UserByEmail matched case-insensitively, and the code must reach the mailbox of
	// record. The login redeem (ConsumeLoginEmailOTP) never reads or writes this
	// address, so the stored casing is authoritative and the row's email snapshot is
	// purely for the audit trail.
	if err := a.Repo.CreateEmailOTP(r.Context(), id, u.ID, u.Email, otpCodeHash(code), otpPurposeLogin, expiresAt); err != nil {
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

// loginEmailVerifyRequest is the verify body: the address and the code read from it.
// Both are needed because there is no principal — the address selects the account,
// the code proves control this session.
type loginEmailVerifyRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// handleLoginEmailVerify redeems a login code into a session (Public, pre-session).
// It resolves the address to an account, verifies the code under otpPurposeLogin, and
// on success mints the same host-only felis_session as handleBindRedeem. A missing account,
// a wrong code, AND an attempt-exhausted (locked) code all return the IDENTICAL 400
// invalid_code, so a code-less caller cannot tell an unknown address from a bad guess
// or farm a lockout into an is-this-a-real-account oracle. Staff are refused — but only
// after a valid code is redeemed, so the refusal is reachable solely by the account
// owner and never leaks which addresses are staff.
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
		// Uniform with a wrong code: a caller probing whether an address has an account
		// gets the same invalid_code either way. (The /auth/options oracle is the
		// sanctioned place to learn existence; this door does not double as one.)
		a.authFailure(r, "login_email", "no_account", nil)
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "email code is invalid or expired"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}

	// Consume the code BEFORE the staff check. Ordering is the whole leak-safety
	// argument: a caller without a valid code always lands in the invalid_code branch
	// below — identical for staff and non-staff — so only the account owner, holding a
	// live code, can ever reach the staff refusal.
	//
	// ConsumeLoginEmailOTP, not VerifyEmailOTP: this door only re-proves control of an
	// already-verified address for the session, so it must NOT rewrite users.email or
	// run the onboarding taken-check. UserByEmail already guaranteed the account is
	// verified; touching the row here would let a stale OTP-snapshot address overwrite
	// the live one and could 500 a correct code on a spurious collision.
	switch err := a.Repo.ConsumeLoginEmailOTP(r.Context(), u.ID, otpPurposeLogin, otpCodeHash(code), a.now()); {
	case errors.Is(err, ErrOTPInvalid), errors.Is(err, ErrOTPLocked), errors.Is(err, ErrOTPAccountLocked):
		// The account lock answers the same way; its owner hears about it by mail.
		a.noteOTPLock(r, err, u.ID, otpPurposeLogin)
		a.authFailure(r, "login_email", otpFailureReason(err), u)
		// Both a wrong/expired code and an attempt-exhausted one return the SAME 400
		// invalid_code, byte-identical to the unknown-account branch above. Surfacing
		// otp_locked as a distinct 429 (as the authenticated onboarding door does) would
		// turn this public door into the existence oracle its no-account branch is
		// careful not to be: a code-less prober could mail a code to a victim address,
		// exhaust the attempt budget, and read otp_locked as "this address has an
		// account." Enumeration is a product decision reserved for /auth/options, not a
		// side channel of the login verify.
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "email code is invalid or expired"))
		return
	case err != nil:
		// ConsumeLoginEmailOTP performs no users write, so ErrEmailTaken is structurally
		// impossible here; anything left is a genuine fault and surfaces as a 500.
		writeError(w, r, err)
		return
	}

	// Code redeemed. Refuse staff here — never before the verify — so op.console keeps
	// its Zero-Trust + in-game-approval gates and this public door provably yields only
	// a role=user player session (mirrors handleBindRedeem's refuse-staff contract).
	// Staff means anything above role=user: an admin OR the role=owner identity. The
	// player door must yield only player sessions.
	if u.Role != "user" {
		a.authFailure(r, "login_email", "staff_account", u)
		writeError(w, r, newError(http.StatusForbidden, "staff_account",
			"that account is staff; sign in at the operator console"))
		return
	}

	token, err := newSessionToken()
	if err != nil {
		writeError(w, r, err)
		return
	}
	expires := a.now().Add(sessionTTL)
	if err := a.Repo.CreateSession(r.Context(), hashCookie(token), u.ID, expires); err != nil {
		writeError(w, r, err)
		return
	}
	setSessionCookie(w, token, expires)
	a.auditAccount(r, u, "auth.login_email", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": u.ID,
		"role":    u.Role,
	})
}
