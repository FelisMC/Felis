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

var (
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("conflict")
	ErrQuotaExceeded      = errors.New("server quota exhausted")
	ErrLinkCodeInvalid    = errors.New("link code invalid or expired")
	ErrConsoleUnavailable = errors.New("server console is unavailable")
	// ErrOTPInvalid conceals whether the code is unknown or the guess is wrong.
	ErrOTPInvalid = errors.New("email code invalid or expired")
	ErrOTPLocked  = errors.New("email code locked: too many attempts")
	// ErrOTPAccountLocked rejects even correct codes until the account-purpose budget lock ends.
	ErrOTPAccountLocked        = errors.New("email codes locked for this account: too many wrong codes")
	ErrPasskeyChallengeInvalid = errors.New("passkey challenge invalid or expired")
	// ErrLastPasskey protects an unverified account's last durable login factor.
	ErrLastPasskey         = errors.New("cannot remove the only passkey of an account without a verified email")
	ErrPlayerBindForbidden = errors.New("bind code belongs to a staff account")
	// ErrPlayerAccountRetired means the account is soft-deleted or disabled; the code is not consumed.
	ErrPlayerAccountRetired = errors.New("player account is retired or disabled")
	// ErrEmailTaken leaves the verification code unconsumed.
	ErrEmailTaken               = errors.New("email already verified on another account")
	ErrTooManyPasskeyChallenges = errors.New("too many passkey login challenges in flight")
	// ErrNotStopped means the server is not fully stopped: desiredState is not Stopped, or the pod still holds the volume (phase Stopping).
	ErrNotStopped            = errors.New("server is not stopped")
	ErrMaintenanceInProgress = errors.New("world maintenance in progress")
)

type MaintenanceBusyError struct{ Kind string }

func (e *MaintenanceBusyError) Error() string {
	return "world maintenance in progress: " + e.Kind
}

func (e *MaintenanceBusyError) Is(target error) bool { return target == ErrMaintenanceInProgress }

// OTPAccountLockedError.JustLocked is true only for the attempt that exhausted the budget.
type OTPAccountLockedError struct {
	Until      time.Time
	JustLocked bool
}

func (e *OTPAccountLockedError) Error() string {
	return ErrOTPAccountLocked.Error() + " until " + e.Until.UTC().Format(time.RFC3339)
}

func (e *OTPAccountLockedError) Is(target error) bool { return target == ErrOTPAccountLocked }

type apiError struct {
	status int
	code   string
	msg    string
	wait   time.Duration // sent as Retry-After, rounded up to whole seconds
}

func (e *apiError) Error() string { return e.msg }

func (e *apiError) retryAfter(d time.Duration) *apiError {
	c := *e
	c.wait = d
	return &c
}

func newError(status int, code, format string, a ...any) *apiError {
	return &apiError{status: status, code: code, msg: fmt.Sprintf(format, a...)}
}

var (
	errUnauthorized    = newError(http.StatusUnauthorized, "unauthorized", "authentication required")
	errAuthUnavailable = newError(http.StatusServiceUnavailable, "auth_unavailable",
		"authentication is temporarily unavailable; retry shortly")
	errForbidden   = newError(http.StatusForbidden, "forbidden", "not permitted")
	errWrongCaller = newError(http.StatusForbidden, "wrong_caller", "this token's caller may not use this route")
	errBadRequest  = newError(http.StatusBadRequest, "bad_request", "invalid request")
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError logs non-apiError diagnostics under request_id and returns an opaque 500.
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
