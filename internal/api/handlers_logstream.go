package api

import (
	"context"
	"errors"
	"net/http"
)

// handleServerConsole streams the caller's server console as Server-Sent Events
// (spec §8, 读写分离: 读=pods/log follow; spec §262 GET /servers/{name}/console #
// SSE). It is the read counterpart to handleCommand (写=RCON): the write side
// dials RCON, this side relays the pod log so the panel sees join/聊天/异步打印
// and live boot progress. Like the write side it is app-tier — your OWN server —
// gated by isOwnerOrAdmin.
//
// The order mirrors handleCommand up to the point of streaming, then diverges:
//
//	① name validation (400)
//	② ownership: owner or admin, else 403 (404 if the server is unknown)
//	③ the nil-Logs guard (503), so a misconstructed API fails clean, not panics
//	④ open the follow stream; map its errors to a normal JSON envelope:
//	     ErrNotFound          → 409 not_running (no running pod to read)
//	     ErrConsoleUnavailable → 503 (a pod exists but its log can't be opened)
//	⑤ relay as SSE (relayLogStream), past which no error envelope is possible
//
// It deliberately does NOT pre-check readiness the way handleCommand does. A
// write only makes sense on a Ready server, but a *read* most wants the logs
// precisely while the server is booting — and §141 readiness IS an RCON probe a
// booting pod fails *while emitting the very boot logs the operator wants to
// watch*. So the read path gates on "is there a running pod" (inside StreamLogs),
// not on RCON readiness.
//
// The RCON password plays no part here at all: the read side never touches it
// (spec §286). The audit row is written BEFORE the relay, because once SSE
// framing begins the handler blocks for the lifetime of the stream — auditing
// after relayLogStream returns would mis-timestamp the attach to the detach.
func (a *API) handleServerConsole(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	// Ownership: owner or admin, mirroring handleCommand. An unknown server is 404.
	rec, err := a.managedServerRecord(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return
	}

	// Logs is wired in production (cmd/felis); the nil guard only defends against a
	// misconstructed API, failing as 503 rather than panicking — same contract as
	// the write side's Console guard.
	if a.Logs == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"console subsystem is not configured"))
		return
	}

	// Bound concurrent SSE streams per principal BEFORE opening the follow stream, so
	// an over-cap caller never even ties up a kube-apiserver connection. A stalled
	// reader keeps this relay (and its upstream follow) alive indefinitely — the write
	// deadline that actually severs it is a separate slice — so this cap is what stops
	// one principal from accumulating unbounded leaked control-plane connections. The
	// slot is held for the whole relay and released on every return path.
	release, ok := a.streamGate().acquire(streamKey(p))
	if !ok {
		writeError(w, r, newError(http.StatusTooManyRequests, "too_many_streams",
			"too many open console streams; close one and retry"))
		return
	}
	defer release()

	// Open the follow stream. Every error must be resolved HERE, into a normal JSON
	// envelope, because relayLogStream commits the 200 + SSE headers and no error
	// body can follow it.
	ctx := r.Context()
	if since, ok := logSinceFromRequest(r, a.now()); ok {
		ctx = withLogSince(ctx, since)
	}
	src, err := a.Logs.StreamLogs(ctx, name)
	switch {
	case errors.Is(err, ErrConsoleUnavailable):
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"server console is currently unreachable; wake the server and retry"))
		return
	case errors.Is(err, ErrNotFound):
		// No running pod to read — the server is stopped or not yet scheduled. This
		// is the read-side analogue of handleCommand's readiness 409.
		writeError(w, r, newError(http.StatusConflict, "not_running",
			"server is not running; wake it before attaching to the console"))
		return
	case err != nil:
		writeError(w, r, newError(http.StatusInternalServerError, "internal",
			"could not open server console"))
		return
	}

	a.audit(r, "console.attach", name)
	relayLogStream(w, r, src, a.streamRecheck(r, func(ctx context.Context, p *Principal) error {
		rec, err := a.managedServerRecord(ctx, name)
		switch {
		case errors.Is(err, ErrNotFound):
			return errForbidden // the server is gone, and the grant with it
		case err != nil:
			return nil
		case !a.isOwnerOrAdmin(p, rec):
			return errForbidden
		}
		return nil
	}), a.streamsClosing())
}

// streamRecheck builds the streamGuard for an external-face stream: it re-runs the
// authentication the stream opened with (the session may have been revoked or
// expired, the account disabled), the op.console staff gate, and then allow for the
// route's own rule. An unreachable session store is not a verdict (see streamGuard).
func (a *API) streamRecheck(r *http.Request, allow func(ctx context.Context, p *Principal) error) streamGuard {
	return func(ctx context.Context) error {
		p, err := a.External.Authenticate(r)
		switch {
		case errors.Is(err, errAuthBackend):
			return nil
		case err != nil || p == nil:
			return errUnauthorized
		case hostIsAdminConsole(r, a.RootDomain, a.AdminHostname) && !p.IsAdmin():
			return errForbidden
		}
		return allow(ctx, p)
	}
}
