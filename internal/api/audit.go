package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"felis.lolicon.best/internal/metrics"
)

// Audit-write failures are logged and counted without failing actions that have already completed.

const (
	// auditWriteTimeout bounds inserts that outlive the request, preventing disconnects from cancelling audit records.
	auditWriteTimeout = 5 * time.Second
	auditUserAgentMax = 256
	anonymousActor    = "anonymous"
)

// auditActor excludes unverified session email addresses to prevent impersonation in audit display names.
func auditActor(p *Principal) string {
	switch {
	case p == nil:
		return anonymousActor
	case p.Email != "" && (p.EmailVerified || (!p.ViaSession && p.APIKey == "")):
		return p.Email
	case p.Username != "":
		return p.Username
	case p.UserID != "":
		return p.UserID
	}
	return anonymousActor
}

// audit stores target in server_name, including non-server object IDs.
func (a *API) audit(r *http.Request, action, target string) {
	p := principalFromContext(r.Context())
	e := AuditEntry{Actor: auditActor(p), Action: action, ServerName: target}
	if p != nil {
		e.ActorUserID = p.UserID
	}
	a.auditEntry(r, e)
}

// auditDetail is audit with a payload.
func (a *API) auditDetail(r *http.Request, action, target string, payload map[string]any) {
	p := principalFromContext(r.Context())
	e := AuditEntry{Actor: auditActor(p), Action: action, ServerName: target, Payload: auditPayload(payload)}
	if p != nil {
		e.ActorUserID = p.UserID
	}
	a.auditEntry(r, e)
}

func (a *API) auditImageChange(r *http.Request, server, from, to string) {
	a.auditDetail(r, "server.patch", server, map[string]any{"image_from": from, "image_to": to})
}

// auditAccount uses the username because pre-session flows have not yet verified the address.
func (a *API) auditAccount(r *http.Request, u *StaffUser, action, target string) {
	e := AuditEntry{Actor: anonymousActor, Action: action, ServerName: target}
	if u != nil {
		e.Actor, e.ActorUserID = u.Username, u.ID
	}
	a.auditEntry(r, e)
}

// auditEntry defaults Source to external; internal callers must set it and their component actor.
func (a *API) auditEntry(r *http.Request, e AuditEntry) {
	external := e.Source == ""
	if external {
		e.Source = "external"
		if p := principalFromContext(r.Context()); p != nil && p.APIKey != "" {
			e.Source = "api_key:" + p.APIKey
		}
	}
	e.RequestID = requestIDFromContext(r.Context())
	if external {
		if ip := a.clientIP(r); ip.IsValid() {
			e.ClientIP = ip.String()
		}
		e.UserAgent = truncateUTF8(r.UserAgent(), auditUserAgentMax)
	}
	a.writeAudit(r.Context(), e)
}

func (a *API) writeAudit(ctx context.Context, e AuditEntry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := a.Repo.Audit(ctx, e); err != nil {
		metrics.AuditWriteFailuresTotal.Inc()
		log.Printf("audit: lost %s by %s (user %q, request_id=%s): %v",
			e.Action, e.Actor, e.ActorUserID, e.RequestID, err)
	}
}

// authFailure records detailed reasons for operators while login responses remain uniform.
func (a *API) authFailure(r *http.Request, door, reason string, u *StaffUser) {
	metrics.AuthFailuresTotal.WithLabelValues(door, reason).Inc()
	e := AuditEntry{Action: "auth." + door + ".failed", Payload: auditPayload(map[string]any{"reason": reason})}
	switch p := principalFromContext(r.Context()); {
	case u != nil:
		e.Actor, e.ActorUserID = cmp.Or(u.Username, u.ID), u.ID
	case p != nil:
		e.Actor, e.ActorUserID = auditActor(p), p.UserID
	default:
		e.Actor = anonymousActor
	}
	a.auditEntry(r, e)
}

// passkeyAssertionRejected records only policy refusals; false leaves unrelated faults for the caller to handle.
func (a *API) passkeyAssertionRejected(r *http.Request, door string, u *StaffUser, credentialID string, err error) bool {
	var reason string
	switch {
	case errors.Is(err, errPasskeyClonedAuthenticator):
		reason = "clone_rejected"
	case errors.Is(err, errPasskeyUserNotVerified):
		reason = "uv_rejected"
	default:
		return false
	}
	metrics.AuthFailuresTotal.WithLabelValues(door, reason).Inc()
	action := "auth.passkey_" + reason
	if u == nil {
		a.audit(r, action, credentialID)
	} else {
		a.auditAccount(r, u, action, credentialID)
	}
	return true
}

func isOTPRefusal(err error) bool {
	return errors.Is(err, ErrOTPInvalid) || errors.Is(err, ErrOTPLocked) || errors.Is(err, ErrOTPAccountLocked)
}

func otpFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrOTPAccountLocked):
		return "account_locked"
	case errors.Is(err, ErrOTPLocked):
		return "code_locked"
	}
	return "bad_code"
}

func auditPayload(v map[string]any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func truncateUTF8(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
