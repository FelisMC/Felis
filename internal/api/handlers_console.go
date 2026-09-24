package api

import (
	"errors"
	"net/http"
	"strings"

	"felis.lolicon.best/internal/naming"
)

// maxConsoleCommandLen caps the command body well under RCON's single-packet
// limit (internal/rcon maxPacketLen = 4096, minus framing). The cap is
// deliberately conservative — interactive console commands are short — so an
// over-long command fails as a clean 400 here rather than surfacing as an opaque
// 500 from the RCON writer.
const maxConsoleCommandLen = 1024

// commandRequest is the console-write body (spec §8 写=RCON). One field, one
// line: decodeJSON rejects unknown fields so no extra knob can smuggle in.
type commandRequest struct {
	Command string `json:"command"`
}

// handleCommand runs one RCON command against the caller's server and returns
// the server's reply (spec §8, 读写分离: 写=RCON; spec §7 POST
// /servers/{name}/command). It is app-tier — operating your OWN server — gated
// by isOwnerOrAdmin, mirroring handleStop. The order is authorization first,
// then a readiness pre-check, then the write:
//
//	① name + body validation (single line, bounded length)
//	② ownership: owner or admin, else 403 (404 if the server is unknown)
//	③ readiness: a write only makes sense on a Running server (§141 Ready ⟺ RCON
//	   reachable), so a non-Ready server is a specific 409, not a blind dial
//	④ the RCON write; an unreachable channel is ErrConsoleUnavailable → 503 (the
//	   §141 invariant can drop between the §③ check and the dial — the pre-check
//	   is for a better error, not a correctness guarantee)
//
// The RCON password is resolved entirely inside the Console implementation and
// never appears in the request or response (spec §286: RCON 密码绝不下发前端).
func (a *API) handleCommand(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	var body commandRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	// A console command is exactly one line. Trim surrounding space, strip a
	// single leading '/' (players type "/say hi"; RCON wants "say hi"), then
	// reject control characters so one request can never smuggle a second command
	// past a newline.
	command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body.Command), "/"))
	if command == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "command is required"))
		return
	}
	if len(command) > maxConsoleCommandLen {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"command too long (max %d bytes)", maxConsoleCommandLen))
		return
	}
	if strings.IndexFunc(command, func(c rune) bool { return c < 0x20 }) >= 0 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"command must be a single line (no control characters)"))
		return
	}

	// Ownership: owner or admin, mirroring handleStop. An unknown server is 404.
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return
	}

	// Readiness pre-check: a write only makes sense on a Running server. This
	// yields a specific "not running" 409 instead of a blind dial that would time
	// out. It is racy (the server can drop between here and the dial), so the
	// write below still maps an unreachable channel to 503.
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !info.Ready {
		writeError(w, r, newError(http.StatusConflict, "not_running",
			"server is not running; wake it before sending console commands"))
		return
	}

	// Console is wired in production (cmd/felis); the nil guard only defends
	// against a misconstructed API, failing as 503 rather than panicking.
	if a.Console == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"console subsystem is not configured"))
		return
	}

	output, err := a.Console.RunCommand(r.Context(), name, command)
	switch {
	case errors.Is(err, ErrConsoleUnavailable):
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"server console is currently unreachable; wake the server and retry"))
		return
	case err != nil:
		// ErrNotFound (server vanished mid-request) → 404; anything else → 500.
		a.writeLookupError(w, r, err)
		return
	}

	a.audit(r, "console.command", name)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "output": output})
}
