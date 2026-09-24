package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
)

// Sentinel errors the repository and cluster layers return so handlers can map
// domain outcomes onto HTTP status codes without leaking driver details.
var (
	// ErrNotFound means the requested server / record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means an atomic precondition failed (e.g. claim lost the race).
	ErrConflict = errors.New("conflict")
	// ErrQuotaExceeded means an ownership write would push the user over a quota
	// cap (spec §9.3). ClaimServer — the atomic gate — returns it when a claim
	// passes the handler's advisory pre-check but loses the serialized re-check
	// (two concurrent claims by one user); handlers map it to a 403
	// quota_exceeded, the same answer the pre-check gives, so the CONCURRENT case
	// and the SEQUENTIAL case are indistinguishable to the caller.
	ErrQuotaExceeded = errors.New("server quota exhausted")
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
	// ErrPlayerBindForbidden means a public Bind-Code redemption resolved to a STAFF
	// account (admin or owner), which the player-console bootstrap refuses
	// (console-tier access model). Staff authenticate at op.console behind Zero Trust,
	// never via the account-less console.<root_domain> door, so the public bootstrap
	// provably never mints a session for a staff identity. It is distinct from
	// ErrConflict so the handler answers 403 (wrong door) rather than 409.
	ErrPlayerBindForbidden = errors.New("bind code belongs to a staff account")
	// ErrPlayerAccountRetired means a Bind-Code redemption resolved to an account the
	// platform has closed: an owner soft-deleted it, or it is disabled (locked out).
	// Reusing the row would mint a fresh session for a dead account — the same
	// resurrection the login doors refuse by resolving only live accounts — so the
	// redeemer gets an explicit 403 instead. The code is NOT consumed, so re-enabling
	// the account and retrying still works within the code's TTL.
	ErrPlayerAccountRetired = errors.New("player account is retired or disabled")
	// ErrEmailTaken means a verified email would collide with another account's
	// already-verified address (spec §B email-first login foundation; the
	// users_verified_email_unique index ships in migration 0020).
	// VerifyEmailOTP returns it — WITHOUT consuming the code, since the address, not
	// the code, is the problem — when a DIFFERENT user has already proven the same
	// address case-insensitively. It is the clean, application-level counterpart of
	// the users_verified_email_unique index: a sequential double-verify meets this
	// guard and gets a 409 instead of a raw unique-violation 500. Distinct from
	// ErrConflict so the message can name the cause (the email is spoken for).
	ErrEmailTaken = errors.New("email already verified on another account")
	// ErrTooManyDiscoverableChallenges means the non-user-keyed discoverable ("usernameless")
	// login challenge store is at its hard cap of live rows (task #40, migration 0013).
	// Unlike the user-keyed enrollment/login challenges — which self-bound via a per-user
	// supersede — a from-zero begin has no principal to key a fair per-caller limit on, so the
	// table is capped globally and a begin over the cap is refused. Distinct from the other
	// sentinels so the handler answers 429 (a transient "too busy, retry" — the cap self-clears
	// as challenges expire), never a 400 that invites an immediate retry.
	ErrTooManyDiscoverableChallenges = errors.New("too many discoverable login challenges in flight")
	// ErrNotStopped means a world-volume operation was refused because the server is
	// not fully stopped: desiredState is not Stopped, or its pod is still shutting
	// down (phase Stopping) and holds the volume while it saves.
	ErrNotStopped = errors.New("server is not stopped")
	// ErrMaintenanceInProgress means another operation holds the server's world
	// volume (internal/maintenance). Cluster methods return it wrapped in a
	// *MaintenanceBusyError that names the holder.
	ErrMaintenanceInProgress = errors.New("world maintenance in progress")
)

// MaintenanceBusyError names what holds a server's world volume. errors.Is
// matches it against ErrMaintenanceInProgress.
type MaintenanceBusyError struct{ Kind string }

func (e *MaintenanceBusyError) Error() string {
	return "world maintenance in progress: " + e.Kind
}

func (e *MaintenanceBusyError) Is(target error) bool { return target == ErrMaintenanceInProgress }

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
	// errAuthUnavailable answers when the session store itself is unreachable
	// (Postgres down): an outage is not a credential verdict, so the caller gets
	// 503 "retry" instead of a 401 that reads as "log in again".
	errAuthUnavailable = newError(http.StatusServiceUnavailable, "auth_unavailable",
		"authentication is temporarily unavailable; retry shortly")
	errForbidden  = newError(http.StatusForbidden, "forbidden", "not permitted")
	errBadRequest = newError(http.StatusBadRequest, "bad_request", "invalid request")
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
//
// That collapse is deliberately lossy on the wire and deliberately NOT lossy in
// the log. Everything the client is denied — the driver message, the wrapped
// chain, the handler that produced it — is written to stderr first, keyed by the
// same request_id the caller is shown. Without that line an operator holding a
// "internal error" has nothing to grep for, and diagnosis degrades into guessing
// against a live install; it cost a full debugging session to learn that once.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		log.Printf("api: %s %s: unmapped error (request_id=%s): %v",
			r.Method, r.URL.Path, requestIDFromContext(r.Context()), err)
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
