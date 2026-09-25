package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
)

// handleHealthz is a liveness probe: the process is up.
func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz is a readiness probe (spec §7). It checks the DB, K8s API and
// CRD informer before declaring ready — a full round-trip that mirrors what the
// actual request path depends on.
func (a *API) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]func(context.Context) error{
		"db":      a.Repo.Ping,
		"k8s_api": a.Cluster.Ping,
	}
	for name, check := range checks {
		if err := check(r.Context()); err != nil {
			// The cause goes to the log; the probe answer names only the dependency,
			// so a driver error (hosts, users, SQL) never reaches a caller.
			log.Printf("api: readyz: %s: %v (request_id=%s)", name, err, requestIDFromContext(r.Context()))
			writeError(w, r, newError(http.StatusServiceUnavailable, "not_ready", "%s is unavailable", name))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// handleListServers serves the velocity registration pull (spec §7 GET /servers):
// the lifecycle view of every MinecraftServer, read from the CRD + status.
func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := a.Cluster.ListServers(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if servers == nil {
		servers = []ServerInfo{} // an empty fleet is [], never null
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

// handleReady accepts a backend's push that a server is up (spec §7
// /internal/servers/{name}/ready). The RCON probe is the authoritative gate, so
// this is advisory: it audits the signal and returns 204.
func (a *API) handleReady(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	a.auditEntry(r, AuditEntry{
		Actor: "backend", Source: internalSource(r), Action: "ready", ServerName: name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// joinEventRequest is the velocity real-player-join report body.
type joinEventRequest struct {
	MCUUID string `json:"mc_uuid"`
}

// handleJoinEvent records a real player join (spec §7 /internal/.../join-event):
// it bumps last_active_at, clears reaper warnings, and auto-appends the UUID to
// the allowlist. This is what keeps an active server alive against the reaper.
func (a *API) handleJoinEvent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	var req joinEventRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.MCUUID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "mc_uuid is required"))
		return
	}
	if err := a.Repo.RecordJoin(r.Context(), name, req.MCUUID); err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// internalWakeRequest is the velocity domain-autostart wake body: the verified
// online-mode UUID of the player whose connection triggered the wake.
type internalWakeRequest struct {
	MCUUID string `json:"mc_uuid"`
}

// handleInternalWake is the internal-face wake (spec §9.1, §14): velocity drives
// domain-autostart with its service token, identifying the joining player by
// online-mode UUID rather than a web Principal. It pulls the same single lever as
// the external wake — autostartPolicy gate, then the shared per-server cooldown,
// then flip the CRD desiredState to Running — and reports the current phase so
// velocity knows whether to hold the player in its waiting queue or transfer
// immediately. The cooldown limiter is shared with the external face, so a wake
// already in flight (whatever its origin) returns 429; velocity treats that as
// "already waking, keep waiting", not a hard failure.
func (a *API) handleInternalWake(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	var req internalWakeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.MCUUID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "mc_uuid is required"))
		return
	}

	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		writeError(w, r, err)
		return
	}

	if err := a.authorizeWakeByUUID(r.Context(), req.MCUUID, info, rec); err != nil {
		writeError(w, r, err)
		return
	}
	if !a.limiter().allowed(name, a.WakeCooldown) {
		writeError(w, r, newError(http.StatusTooManyRequests, "cooldown", "wake is cooling down, retry shortly"))
		return
	}
	// Global running-server cap (spec §9.1), shared with the external wake. velocity
	// treats 503 at_capacity as "cluster full, tell the player to try later" and does
	// NOT enqueue them (nothing is coming up, so waiting would only strand them),
	// distinct from the 429 cooldown's "already waking, keep waiting".
	ok, err := a.withinRunningCap(r.Context(), info)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, newError(http.StatusServiceUnavailable, "at_capacity",
			"the cluster is at its running-server cap; retry once a server stops"))
		return
	}

	// A 409 maintenance_in_progress tells velocity nothing is coming up until the
	// restore/backup/file write finishes, so it does not enqueue the player.
	if err := a.Cluster.SetDesiredState(r.Context(), name, v1alpha1.DesiredRunning); err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	// Consume the shared per-server cooldown only after the wake flips, so a join
	// the cap held with 503 (or a SetDesiredState error) leaves the cooldown
	// untouched and the next join attempt is not also throttled.
	a.limiter().record(name)
	a.auditEntry(r, AuditEntry{
		Actor: "velocity", Source: internalSource(r), Action: "wake", ServerName: name,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"name": name, "desiredState": "Running",
		"phase": info.Phase, "ready": info.Ready,
	})
}

// internalClaimRequest is the velocity `Claim & Start` body: the verified
// online-mode UUID of the player claiming an ownerless server (spec §9.3, §12).
type internalClaimRequest struct {
	MCUUID string `json:"mc_uuid"`
}

// handleInternalClaim is the internal-face claim (spec §9.3, §12): the lobby's
// `Claim & Start` button drives it through velocity, identifying the claiming
// player by their verified online-mode UUID rather than a web Principal. It pulls
// the same atomic UPDATE...WHERE owner_id IS NULL lever as the external claim and
// the same quota gate, but resolves identity by UUID. The two operations §12
// describes — claim then wake — stay separate on purpose: claim needs a link plus
// quota (here), wake needs the autostartPolicy gate (handleInternalWake); velocity
// follows a 200 here with a wake call. An unlinked UUID can own nothing, so it is
// the internal-face equivalent of the external claim's 412 not_linked, distinct
// from a 404 for a missing server.
func (a *API) handleInternalClaim(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	var req internalClaimRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.MCUUID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "mc_uuid is required"))
		return
	}

	// ① resolve identity by UUID. An unlinked UUID (no account_links row) cannot
	// establish ownership; a successful resolve already implies linked, so there is
	// no separate IsLinked check (mirrors the external claim's order, link → quota
	// → write). ErrNotFound here is "claimer not linked" (412), never "server
	// missing" — that distinction is the claim call's, below.
	userID, err := a.Repo.UserByMCUUID(r.Context(), req.MCUUID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusPreconditionFailed, "not_linked",
				"link your Minecraft account before claiming (see /api/v1/account/link/start)"))
			return
		}
		writeError(w, r, err)
		return
	}

	// ② quota gate, evaluated before the ownership write (mirrors handleClaim).
	// All four dimensions (servers, CPU, memory, storage) are checked.
	res, err := a.Repo.ServerResources(r.Context(), name)
	if err != nil {
		writeError(w, r, err)
		return
	}
	ok, err := a.Repo.QuotaCheck(r.Context(), userID, "", res)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, newError(http.StatusForbidden, "quota_exceeded", "server quota exhausted"))
		return
	}

	// ③ atomic claim. A missing server is 404 (writeLookupError), distinct from the
	// 412 above; a lost race (0 rows) is 409.
	claimed, err := a.Repo.ClaimServer(r.Context(), name, userID)
	if err != nil {
		// Same atomic quota gate as the external face (audit #4): the concurrent
		// loser gets the sequential 403, never an over-provisioned tenant.
		if errors.Is(err, ErrQuotaExceeded) {
			writeError(w, r, newError(http.StatusForbidden, "quota_exceeded", "server quota exhausted"))
			return
		}
		a.writeLookupError(w, r, err)
		return
	}
	if !claimed {
		writeError(w, r, newError(http.StatusConflict, "already_claimed", "server is already claimed"))
		return
	}

	a.auditEntry(r, AuditEntry{
		Actor: "velocity", Source: internalSource(r), Action: "claim", ServerName: name,
	})
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "claimed": true})
}

// handleInternalMenuStatus is the lobby `/menu` projection (spec §12): everything
// the lobby GUI needs to render one server tile, composed from the lifecycle view
// (phase/ready/players from the CRD status) and the business ownership row
// (claimable = nobody owns it yet). It is the only internal response carrying
// claimable, so it has its own shape — the §11 list/status views never
// expose ownership, and folding owner data into ServerInfo would force the
// lifecycle layer to consult Postgres.
//
// claimable is ownership-only and UUID-independent: it reports whether the server
// is ownerless, not whether *this* player may claim it (the link + quota gates are
// the claim call's, not the menu's). The lobby uses it purely to choose between
// rendering `Claim & Start` (ownerless) and `Join`/`Wake` (owned). A server known
// to the cluster but missing its servers-row is treated as ownerless, so it still
// renders a sane tile rather than erroring.
func (a *API) handleInternalMenuStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	claimable := true
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		writeError(w, r, err)
		return
	}
	if rec != nil && rec.OwnerID != "" {
		claimable = false
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":          name,
		"phase":         info.Phase,
		"ready":         info.Ready,
		"playersOnline": info.PlayersOnline,
		"playersMax":    info.PlayersMax,
		"claimable":     claimable,
	})
}

// authorizeWakeByUUID is the internal-face counterpart of authorizeWake (spec
// §9.4): it applies the autostartPolicy gate for a wake driven by velocity, where
// the joining player is known only by their verified online-mode UUID rather than
// a web Principal. The admin tier rides the same trust anchor as the op-login
// approve — online-mode auth plus the account link plus the stored staff role —
// so a linked administrator wakes ANY node without claiming it, mirroring the
// external gate's IsAdmin bypass. The owner bypass applies as before, and an
// unlinked UUID (no account_links row) carries no standing at all and falls
// through to the policy gate, so ownerOnly/unset fails safe exactly as on the
// web face.
func (a *API) authorizeWakeByUUID(ctx context.Context, mcUUID string, info *ServerInfo, rec *ServerRecord) error {
	// public needs no identity at all — skip the account_links resolution.
	if info.AutostartPolicy == string(v1alpha1.AutostartPublic) {
		return nil
	}
	// Resolve the UUID to its linked user once; staff role or ownership grants
	// the bypass. A missing link is not an error here — it just means "no
	// standing", and a link pointing at a vanished user reads the same way.
	switch userID, err := a.Repo.UserByMCUUID(ctx, mcUUID); {
	case err == nil:
		switch u, err := a.Repo.UserByID(ctx, userID); {
		case err == nil:
			if staffRole(u.Role) {
				return nil
			}
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if rec != nil && rec.OwnerID != "" && userID == rec.OwnerID {
			return nil
		}
	case errors.Is(err, ErrNotFound):
		// unlinked UUID → fall through to the policy gate
	default:
		return err
	}
	switch info.AutostartPolicy {
	case string(v1alpha1.AutostartAllowlist):
		ok, err := a.Repo.UUIDInAllowlist(ctx, info.Name, mcUUID)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		return errForbidden
	default: // ownerOnly or unset → only the owner (handled above) may wake
		return errForbidden
	}
}

// writeLookupError maps a repo/cluster lookup error onto an HTTP status: a
// missing record is 404, an atomic precondition failure is 409, anything else is
// an opaque 500.
func (a *API) writeLookupError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, r, newError(http.StatusNotFound, "not_found", "not found"))
	case errors.Is(err, ErrConflict):
		writeError(w, r, newError(http.StatusConflict, "conflict", "conflict"))
	case errors.Is(err, ErrMaintenanceInProgress), errors.Is(err, ErrNotStopped):
		writeError(w, r, maintenanceError(err, "stop the server completely first"))
	default:
		writeError(w, r, err)
	}
}
