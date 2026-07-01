package api

import (
	"net/http"

	"golang.org/x/crypto/bcrypt"
)

// Local-password auth handlers (spec §B). Owner/Operator log in to op.console with
// username+password when Zero Trust is not in front of the API (the demo's primary
// web login, and the always-available break-glass-enabled path). These three
// handlers are the whole surface: log in, log out, change password. `felis
// breakGlass` mints/resets the credentials direct-to-Postgres; the panel never
// creates a staff account.

// bcryptCost is the work factor for every password hash we write. It is read back
// from each stored hash on compare, so raising it later re-hashes lazily on the
// next change without invalidating existing hashes.
const bcryptCost = bcrypt.DefaultCost

// dummyPasswordHash is a real bcrypt hash, at bcryptCost, of a throwaway value. A
// failed login (unknown username, or a player row with no password) compares the
// supplied password against it anyway, so the response time matches a real
// password check and cannot be used to enumerate which usernames carry a password.
// It is computed once at init — real and same-cost, never a short-circuit — and
// the throwaway value is never a valid credential because the surrounding logic
// rejects any login whose user has no stored hash regardless of the compare.
var dummyPasswordHash = mustDummyHash()

func mustDummyHash() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("felis-anti-enumeration-placeholder"), bcryptCost)
	if err != nil {
		panic("bcrypt dummy hash: " + err.Error())
	}
	return h
}

// loginRequest is the op.console login form.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin verifies a username+password against the users row and, on success,
// mints a server-side session cookie (spec §B). It is mounted Public — there is no
// prior principal — but still requires local auth to be enabled, so a deployment
// fronted entirely by Zero Trust never accepts a local password. Every failure
// returns the same vague errInvalidCredentials after a uniform bcrypt compare.
func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"local password login is disabled"))
		return
	}
	// Reject a non-JSON body before decoding: this is the public, credential-minting
	// route, so it is the cross-site-forgery surface requireJSONContentType closes.
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}

	var body loginRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Username == "" || body.Password == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "username and password are required"))
		return
	}

	u, err := a.Repo.UserByUsername(r.Context(), body.Username)
	if err != nil && !errIsNotFound(err) {
		writeError(w, r, err)
		return
	}

	// Anti-enumeration: always run a bcrypt compare, even on a missing user or a
	// player row (empty hash), against the dummy hash. The trailing guard makes the
	// missing-hash cases fail closed even if a caller supplied the dummy's plaintext.
	hash := dummyPasswordHash
	if u != nil && u.PasswordHash != "" {
		hash = []byte(u.PasswordHash)
	}

	// Bound concurrent bcrypt: this public route runs a full-cost compare on every
	// request (the anti-enumeration dummy included), so an unbounded flood of
	// simultaneous logins would pin every core. Take one of a fixed number of compare
	// slots and shed the excess with a 429 rather than adding to the CPU pile. The
	// slot guards only the hash — it is released the instant the compare returns,
	// before the session I/O — and being a concurrency cap (not a per-username
	// lockout) it never fences the break-glass admin out. The 429 lands before any
	// credential distinction, so it leaks nothing about the username either.
	release, ok := a.loginLimiter().acquire()
	if !ok {
		writeError(w, r, newError(http.StatusTooManyRequests, "auth_busy",
			"authentication is busy; retry in a moment"))
		return
	}
	matched := bcrypt.CompareHashAndPassword(hash, []byte(body.Password)) == nil
	release()
	if !matched || u == nil || u.PasswordHash == "" {
		writeError(w, r, errInvalidCredentials)
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
	a.audit(r, u.Username, "auth.login", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":              u.ID,
		"role":                 u.Role,
		"must_change_password": u.MustChangePassword,
	})
}

// handleLogout revokes the presented session and clears the cookie (spec §B). It
// is mounted Public and idempotent: it reads the cookie directly, so it works even
// when the session has already expired and never errors on a missing one.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		_ = a.Repo.RevokeSession(r.Context(), hashCookie(c.Value))
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// changePasswordRequest is the change-password form.
type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword re-verifies the caller's current password, stores a new
// bcrypt hash, clears must_change_password, and revokes the account's OTHER
// sessions while keeping the current one (spec §B). It is reachable while
// must_change_password is set (AllowDuringPasswordChange) so a forced first-login
// change can complete. The session itself authenticates the caller; re-asking the
// current password additionally blocks a hijacked session from silently rotating
// the credential.
func (a *API) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())

	// Defense-in-depth: this route is already CSRF-safe (a session is required, the
	// cookie is SameSite=Lax, and the current password is re-verified below), but the
	// same content-type guard keeps every local-auth JSON write uniform.
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}

	var body changePasswordRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if err := validateNewPassword(body.NewPassword); err != nil {
		writeError(w, r, err)
		return
	}

	u, err := a.Repo.UserByID(r.Context(), p.UserID)
	switch {
	case errIsNotFound(err):
		// The session resolved a moment ago but the user is gone: treat as unauthenticated.
		writeError(w, r, errUnauthorized)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	if u.PasswordHash == "" {
		// A link-only account has no password to change — it never reaches this path
		// in practice, but fail closed rather than set a first password here.
		writeError(w, r, errForbidden)
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(body.CurrentPassword)) != nil {
		writeError(w, r, newError(http.StatusUnauthorized, "invalid_credentials", "current password is incorrect"))
		return
	}
	// The new password must actually differ from the current one.
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(body.NewPassword)) == nil {
		writeError(w, r, newError(http.StatusBadRequest, "password_unchanged",
			"new password must differ from the current one"))
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcryptCost)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.Repo.SetPassword(r.Context(), u.ID, string(newHash)); err != nil {
		writeError(w, r, err)
		return
	}

	// Log out the account's other devices, keeping the current session. The current
	// session is identified by the cookie hash; with no cookie (no live session to
	// keep) every session of the user is revoked, which is the safe direction.
	keep := ""
	if c, cerr := r.Cookie(sessionCookieName); cerr == nil {
		keep = hashCookie(c.Value)
	}
	if err := a.Repo.RevokeUserSessionsExcept(r.Context(), u.ID, keep); err != nil {
		writeError(w, r, err)
		return
	}

	// Revoking sessions is not enough: a passkey needs no password, so one planted
	// through a transiently-hijacked session would outlive the reset as a standing login
	// foothold. A password change is a possible-compromise signal, so unbind every passkey
	// as part of the same remediation. The user re-enrolls afterward if they want one; the
	// email-OTP factor stays available in the meantime, so this never locks anyone out.
	if err := a.Repo.DeleteAllPasskeyCredentialsForUser(r.Context(), u.ID); err != nil {
		writeError(w, r, err)
		return
	}

	a.audit(r, u.Username, "auth.password_change", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// validateNewPassword enforces the minimal password policy: 8–72 bytes. The upper
// bound is bcrypt's hard limit (it errors past 72 bytes), surfaced here as a clean
// 400 rather than an opaque 500 from GenerateFromPassword.
func validateNewPassword(pw string) error {
	if len(pw) < 8 {
		return newError(http.StatusBadRequest, "weak_password", "password must be at least 8 characters")
	}
	if len(pw) > 72 {
		return newError(http.StatusBadRequest, "weak_password", "password must be at most 72 bytes")
	}
	return nil
}
