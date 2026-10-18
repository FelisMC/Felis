package api

import (
	"context"
	"errors"
	"net/http"
)

// handleServerConsole does not require RCON readiness so boot logs remain available before the server is ready.
func (a *API) handleServerConsole(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	rec, err := a.managedServerRecord(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return
	}

	if a.Logs == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"console subsystem is not configured"))
		return
	}

	// Reserve the stream slot before opening the upstream connection and retain it for the relay's lifetime.
	release, ok := a.streamGate().acquire(streamKey(p))
	if !ok {
		writeError(w, r, newError(http.StatusTooManyRequests, "too_many_streams",
			"too many open console streams; close one and retry"))
		return
	}
	defer release()

	// Resolve errors before relayLogStream commits the SSE headers.
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
			return errForbidden
		case err != nil:
			return nil
		case !a.isOwnerOrAdmin(p, rec):
			return errForbidden
		}
		return nil
	}), a.streamsClosing())
}

// streamRecheck repeats authentication, operator-host checks and route authorization; store outages do not revoke access.
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
