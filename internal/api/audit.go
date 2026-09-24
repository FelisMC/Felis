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

// Audit rows (audit_logs, spec §6).
//
// Every row an HTTP request writes carries the acting account's id
// (actor_user_id), the caller's address (the one the sign-in limit keys on) and
// user agent; actor is display text. A failed write never fails the operation
// it records, which already happened, but it is logged and counted
// (felis_audit_write_failures_total, FelisAuditWriteFailing): a silent drop is
// how a database blip erases the trail.

const (
	// auditWriteTimeout bounds one audit insert. The write outlives the caller's
	// request context, so a client that hangs up right after the action cannot
	// cancel its own audit row.
	auditWriteTimeout = 5 * time.Second
	// auditUserAgentMax bounds the stored user agent; the header is the caller's
	// to write.
	auditUserAgentMax = 256
	// anonymousActor names a caller no account was resolved for.
	anonymousActor = "anonymous"
)

// auditActor is the display name for a principal: an email only when something
// vouches for it (an Access JWT, or a session whose address was verified), else
// the username. A player can set their address to anyone's before verifying it,
// so an unverified email would let them sign rows as that person.
func auditActor(p *Principal) string {
	switch {
	case p == nil:
		return anonymousActor
	case p.Email != "" && (p.EmailVerified || !p.ViaSession):
		return p.Email
	case p.Username != "":
		return p.Username
	case p.UserID != "":
		return p.UserID
	}
	return anonymousActor
}

// audit records an action by the signed-in caller. target is the object acted
// on (a server name, a user or credential id) and lands in server_name.
func (a *API) audit(r *http.Request, action, target string) {
	p := principalFromContext(r.Context())
	e := AuditEntry{Actor: auditActor(p), Action: action, ServerName: target}
	if p != nil {
		e.ActorUserID = p.UserID
	}
	a.auditEntry(r, e)
}

// auditImageChange records a confirmed image change as server.patch with the
// image it replaced and the one it set, so the audit log alone can say which
// build a world ran before it was moved.
func (a *API) auditImageChange(r *http.Request, server, from, to string) {
	p := principalFromContext(r.Context())
	e := AuditEntry{Actor: auditActor(p), Action: "server.patch", ServerName: server}
	if p != nil {
		e.ActorUserID = p.UserID
	}
	e.Payload = auditPayload(map[string]any{"image_from": from, "image_to": to})
	a.auditEntry(r, e)
}

// auditAccount records an action a pre-session door took for the account it
// resolved (u nil: none was). The username is the actor: the door has not yet
// proven anything about the address.
func (a *API) auditAccount(r *http.Request, u *StaffUser, action, target string) {
	e := AuditEntry{Actor: anonymousActor, Action: action, ServerName: target}
	if u != nil {
		e.Actor, e.ActorUserID = u.Username, u.ID
	}
	a.auditEntry(r, e)
}

// auditEntry fills the request detail into e and writes it. Source defaults to
// external; internal callers set it and the component actor themselves.
func (a *API) auditEntry(r *http.Request, e AuditEntry) {
	if e.Source == "" {
		e.Source = "external"
	}
	e.RequestID = requestIDFromContext(r.Context())
	if e.Source == "external" {
		if ip := a.clientIP(r); ip.IsValid() {
			e.ClientIP = ip.String()
		}
		e.UserAgent = truncateUTF8(r.UserAgent(), auditUserAgentMax)
	}
	a.writeAudit(r.Context(), e)
}

// writeAudit inserts e, logging and counting a failure.
func (a *API) writeAudit(ctx context.Context, e AuditEntry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := a.Repo.Audit(ctx, e); err != nil {
		metrics.AuditWriteFailuresTotal.Inc()
		log.Printf("audit: lost %s by %s (user %q, request_id=%s): %v",
			e.Action, e.Actor, e.ActorUserID, e.RequestID, err)
	}
}

// authFailure records one refused sign-in attempt: felis_auth_failures_total
// by door and reason, and an auth.<door>.failed row naming the account when
// the door resolved one (u nil: the signed-in caller if any, else anonymous).
// The doors keep their answers uniform so a prober learns nothing; the reason
// is for the operator.
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

// passkeyCloneRejected records an assertion refused for a regressed signature
// counter. It keeps its own action so a cloned authenticator stands out from
// ordinary failures, and counts as a failure of its door. u nil: a signed-in
// step-up, attributed to the caller.
func (a *API) passkeyCloneRejected(r *http.Request, door string, u *StaffUser, credentialID string) {
	metrics.AuthFailuresTotal.WithLabelValues(door, "clone_rejected").Inc()
	if u == nil {
		a.audit(r, "auth.passkey_clone_rejected", credentialID)
		return
	}
	a.auditAccount(r, u, "auth.passkey_clone_rejected", credentialID)
}

// isOTPRefusal reports whether err is a refused code (wrong, spent, or the
// account's budget locked), as opposed to a fault.
func isOTPRefusal(err error) bool {
	return errors.Is(err, ErrOTPInvalid) || errors.Is(err, ErrOTPLocked) || errors.Is(err, ErrOTPAccountLocked)
}

// otpFailureReason names a refused code for authFailure.
func otpFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrOTPAccountLocked):
		return "account_locked"
	case errors.Is(err, ErrOTPLocked):
		return "code_locked"
	}
	return "bad_code"
}

// auditPayload marshals a small detail map for AuditEntry.Payload.
func auditPayload(v map[string]any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// truncateUTF8 makes s valid UTF-8 (a header may carry any byte, a text
// column refuses invalid sequences) and cuts it to at most n bytes on a rune
// boundary.
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
