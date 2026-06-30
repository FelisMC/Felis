package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors the repository and cluster layers return so handlers can map
// domain outcomes onto HTTP status codes without leaking driver details.
var (
	// ErrNotFound means the requested server / record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means an atomic precondition failed (e.g. claim lost the race).
	ErrConflict = errors.New("conflict")
	// ErrLinkCodeInvalid means an account-link code is unknown or expired (spec
	// §10). It is a client error (the verify endpoint exists; the code is bad), so
	// handlers map it to 400, not 404.
	ErrLinkCodeInvalid = errors.New("link code invalid or expired")
	// ErrConsoleUnavailable means the RCON write channel could not be reached —
	// the dial timed out, was refused, or the password was rejected (spec §8).
	// Because readiness IS an RCON probe (spec §141: phase=Running ⟺ RCON
	// answers), a reachable failure here is a transient/racy "the server isn't
	// actually up", not a server bug. Handlers map it to 503, not 500, so the
	// caller is told to wake/retry rather than shown an opaque internal error.
	ErrConsoleUnavailable = errors.New("server console is unavailable")
	// ErrOTPInvalid means an email one-time code is unknown, expired, already
	// consumed, or did not match (spec §B2 onboarding). Like ErrLinkCodeInvalid it
	// is a client error — the verify endpoint exists; the code is bad — so handlers
	// map it to 400, not 404. A wrong-but-not-yet-locked guess collapses to it too,
	// so the response never distinguishes "no such code" from "wrong digits".
	ErrOTPInvalid = errors.New("email code invalid or expired")
	// ErrOTPLocked means the live email code has exhausted its attempt budget: too
	// many wrong guesses (spec §B2). It is distinct from ErrOTPInvalid so handlers
	// can answer 429 (back off / request a new code) rather than inviting another
	// guess against a code that will never accept one.
	ErrOTPLocked = errors.New("email code locked: too many attempts")
	// ErrPasskeyChallengeInvalid means a passkey enrollment ceremony cannot be
	// finished: there is no live (unconsumed, unexpired) challenge for the caller and
	// purpose (Phase 6 WebAuthn bind). Like ErrOTPInvalid it is a client error — the
	// finish endpoint exists; the ceremony state is gone (never begun, already
	// consumed, or expired) — so handlers map it to 400, not 404.
	ErrPasskeyChallengeInvalid = errors.New("passkey challenge invalid or expired")
)

// apiError is a handler-level error carrying an HTTP status and a stable,
// machine-readable code. The error envelope matches the platform convention:
//
//	{"error": {"code": "...", "message": "...", "request_id": "..."}}
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// newError builds an apiError with a formatted message.
func newError(status int, code, format string, a ...any) *apiError {
	return &apiError{status: status, code: code, msg: fmt.Sprintf(format, a...)}
}

// Common errors reused across handlers.
var (
	errUnauthorized = newError(http.StatusUnauthorized, "unauthorized", "authentication required")
	errForbidden    = newError(http.StatusForbidden, "forbidden", "not permitted")
	errBadRequest   = newError(http.StatusBadRequest, "bad_request", "invalid request")
	// errInvalidCredentials is the single, deliberately vague answer to any failed
	// local-password login (spec §B): unknown username, player row, or wrong
	// password all collapse to it so the response never reveals which usernames
	// carry a password. The anti-enumeration dummy-hash compare keeps the timing
	// uniform alongside it (handlers_auth.go).
	errInvalidCredentials = newError(http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
	// errPasswordChangeRequired fences a staff principal that still owes a
	// first-login password change to the change-password surface. The lockdown
	// middleware returns it from every authenticated route except the opt-out set
	// (change-password / logout / me), so a half-onboarded account cannot act until
	// it sets its own password.
	errPasswordChangeRequired = newError(http.StatusForbidden, "password_change_required",
		"change your password before continuing")
)

// writeJSON writes v as an indented JSON body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError renders err as the standard error envelope. Non-apiError values
// collapse to a 500 so driver/internal details never reach the client.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = newError(http.StatusInternalServerError, "internal", "internal error")
	}
	body := map[string]any{
		"error": map[string]string{
			"code":       ae.code,
			"message":    ae.msg,
			"request_id": requestIDFromContext(r.Context()),
		},
	}
	writeJSON(w, ae.status, body)
}
