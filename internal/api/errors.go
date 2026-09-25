package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
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
	// ErrOTPAccountLocked means the (user, purpose) has spent its wrong-code budget
	// for the current window (otpFailureBudget): every code for that door is refused,
	// the right one included, until the window ends. The repo returns it as an
	// *OTPAccountLockedError carrying the end of the lock.
	ErrOTPAccountLocked = errors.New("email codes locked for this account: too many wrong codes")
	// ErrPasskeyChallengeInvalid means a passkey enrollment ceremony cannot be
	// finished: there is no live (unconsumed, unexpired) challenge for the caller and
	// purpose (Phase 6 WebAuthn bind). Like ErrOTPInvalid it is a client error — the
	// finish endpoint exists; the ceremony state is gone (never begun, already
	// consumed, or expired) — so handlers map it to 400, not 404.
	ErrPasskeyChallengeInvalid = errors.New("passkey challenge invalid or expired")
	// ErrLastPasskey means a passkey delete would remove the account's only one while
	// its email is unverified. That passkey is then the account's only durable way
	// in (setupRequired: no verified email and no passkey puts it back behind the
	// setup gate, and a staff account has no other self-service door at all), so the
	// delete is refused; handlers map it to 409 last_passkey.
	ErrLastPasskey = errors.New("cannot remove the only passkey of an account without a verified email")
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
	// ErrTooManyPasskeyChallenges means a passkey login begin was refused because too many
	// login challenges are live: the caller's source already holds its allowance
	// (maxLiveChallengesPerSource), or the discoverable store is at its global cap
	// (maxLiveDiscoverableChallenges). Distinct from the other sentinels so the handler
	// answers 429 (a transient "too busy, retry" — both bounds clear as challenges expire),
	// never a 400 that invites an immediate retry.
	ErrTooManyPasskeyChallenges = errors.New("too many passkey login challenges in flight")
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

// OTPAccountLockedError is ErrOTPAccountLocked with its detail. JustLocked is set
// only on the wrong guess that spent the budget, so the handler notifies and
// audits the lock exactly once.
type OTPAccountLockedError struct {
	Until      time.Time
	JustLocked bool
}

func (e *OTPAccountLockedError) Error() string {
	return ErrOTPAccountLocked.Error() + " until " + e.Until.UTC().Format(time.RFC3339)
}

func (e *OTPAccountLockedError) Is(target error) bool { return target == ErrOTPAccountLocked }

// apiError is a handler-level error carrying an HTTP status and a stable,
// machine-readable code. The error envelope matches the platform convention:
//
//	{"error": {"code": "...", "message": "...", "request_id": "..."}}
type apiError struct {
	status int
	code   string
	msg    string
	// wait, when positive, is sent as Retry-After (whole seconds, rounded up).
	wait time.Duration
}

func (e *apiError) Error() string { return e.msg }

// retryAfter returns a copy of e that tells the client when to retry.
func (e *apiError) retryAfter(d time.Duration) *apiError {
	c := *e
	c.wait = d
	return &c
}

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
	errForbidden = newError(http.StatusForbidden, "forbidden", "not permitted")
	// errWrongCaller: a valid internal token for a caller this route does not serve.
	errWrongCaller = newError(http.StatusForbidden, "wrong_caller", "this token's caller may not use this route")
	errBadRequest  = newError(http.StatusBadRequest, "bad_request", "invalid request")
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
	if ae.wait > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64((ae.wait+time.Second-1)/time.Second), 10))
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
