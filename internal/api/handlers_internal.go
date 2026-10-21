package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
)

func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]func(context.Context) error{
		"db":      a.Repo.Ping,
		"k8s_api": a.Cluster.Ping,
	}
	for name, check := range checks {
		if err := check(r.Context()); err != nil {
			// Return only the failed dependency's name; driver diagnostics may expose database or host details.
			log.Printf("api: readyz: %s: %v (request_id=%s)", name, err, requestIDFromContext(r.Context()))
			writeError(w, r, newError(http.StatusServiceUnavailable, "not_ready", "%s is unavailable", name))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := a.Cluster.ListServers(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if servers == nil {
		servers = []ServerInfo{} // encode an empty fleet as []
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

// handleReady is advisory; the RCON probe remains authoritative.
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

type joinEventRequest struct {
	MCUUID string `json:"mc_uuid"`
}

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
	mcUUID, err := parseMCUUID(req.MCUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.Repo.RecordJoin(r.Context(), name, mcUUID); err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type internalWakeRequest struct {
	MCUUID string `json:"mc_uuid"`
}

// handleInternalWake shares the external wake cooldown; 429 means Velocity can keep waiting for the existing start.
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
	mcUUID, err := parseMCUUID(req.MCUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	// Joining an already-running server requires no wake policy check or cooldown.
	if info.Ready && info.DesiredState == string(v1alpha1.DesiredRunning) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"name": name, "desiredState": info.DesiredState,
			"phase": info.Phase, "ready": true,
		})
		return
	}
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		writeError(w, r, err)
		return
	}

	if err := a.authorizeWakeByUUID(r.Context(), mcUUID, info, rec); err != nil {
		writeError(w, r, err)
		return
	}
	if rec != nil && rec.Retire != nil {
		writeError(w, r, errServerRetiring)
		return
	}
	// Exhausted restarts require human intervention; joins must not reset the restart budget or enqueue players.
	if info.StartGaveUp && info.DesiredState == string(v1alpha1.DesiredRunning) {
		writeError(w, r, newError(http.StatusConflict, "start_failed",
			"the server failed to start and its automatic retries are spent; its owner can retry from the panel"))
		return
	}
	if info.CrashStopped {
		writeError(w, r, errCrashStopped)
		return
	}
	policy, _, err := a.readWakePolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !a.limiter().allowed(name, policy.cooldown(a.WakeCooldown)) {
		writeError(w, r, newError(http.StatusTooManyRequests, "cooldown", "wake is cooling down, retry shortly"))
		return
	}
	// Capacity refusal is 503, distinct from cooldown's 429; Velocity must not enqueue players when no startup is pending.
	ok, err := a.withinRunningCap(r.Context(), info, policy.MaxRunningServers)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, newError(http.StatusServiceUnavailable, "at_capacity",
			"the cluster is at its running-server cap; retry once a server stops"))
		return
	}

	// Reaping differs from temporary maintenance: completion releases the server with an empty world.
	if err := a.Cluster.SetDesiredState(r.Context(), name, v1alpha1.DesiredRunning); err != nil {
		var busy *MaintenanceBusyError
		if errors.As(err, &busy) && busy.Kind == maintenance.KindReap {
			writeError(w, r, errWorldReclaiming)
			return
		}
		a.writeLookupError(w, r, err)
		return
	}
	// Consume the cooldown only after a successful wake so capacity refusals and write failures do not throttle retries.
	a.limiter().record(name)
	a.auditEntry(r, AuditEntry{
		Actor: "velocity", Source: internalSource(r), Action: "wake", ServerName: name,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"name": name, "desiredState": "Running",
		"phase": info.Phase, "ready": info.Ready,
	})
}

// errCrashStopped shares start_failed with spent start retries, so the proxy and lobby menu show the same notice.
var errCrashStopped = newError(http.StatusConflict, "start_failed",
	"the server stopped after crashing and no longer restarts on its own; its owner can start it from the panel")

var errWorldReclaiming = newError(http.StatusConflict, "world_reclaiming",
	"this server sat idle too long and its world is being archived; afterwards it is released with an empty world")

type internalClaimRequest struct {
	MCUUID string `json:"mc_uuid"`
}

// handleInternalClaim resolves the linked game UUID and claims atomically; waking remains a separate policy-gated call.
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
	mcUUID, err := parseMCUUID(req.MCUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// An unlinked claimant is 412, distinct from a missing server's 404.
	userID, err := a.Repo.UserByMCUUID(r.Context(), mcUUID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusPreconditionFailed, "not_linked",
				"link your Minecraft account before claiming (see /api/v1/account/link/start)"))
			return
		}
		writeError(w, r, err)
		return
	}

	res, err := a.claimResources(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
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

	claimed, err := a.Repo.ClaimServer(r.Context(), name, userID)
	if err != nil {
		// The atomic claim repeats the quota gate so concurrent requests cannot exceed the tenant's allowance.
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

// handleInternalMenuStatus reports ownership-only claimability; link and quota eligibility are checked when claiming.
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
	if rec != nil && (rec.OwnerID != "" || rec.Retire != nil) {
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

const (
	menuRetiring    = "retiring"
	menuStartFailed = "start_failed"
	menuOwner       = "owner"
	menuWake        = "wake"
	menuOwnerOnly   = "owner_only"
	menuAllowlist   = "allowlist"
)

// handleInternalMenuAccess projects wake eligibility without transient cooldown and capacity refusals.
func (a *API) handleInternalMenuAccess(w http.ResponseWriter, r *http.Request) {
	mcUUID, err := parseMCUUID(r.PathValue("mc_uuid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	infos, err := a.Cluster.ListServers(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	owners, err := a.Repo.ServerOwners(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	standing, err := a.standingByUUID(r.Context(), mcUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	verdicts := make(map[string]string, len(infos))
	for i := range infos {
		info := &infos[i]
		if naming.ValidateServerName(info.Name) != nil {
			continue
		}
		own := owners[info.Name]
		switch {
		case own.Retire != nil:
			verdicts[info.Name] = menuRetiring
		case info.StartGaveUp && info.DesiredState == string(v1alpha1.DesiredRunning), info.CrashStopped:
			verdicts[info.Name] = menuStartFailed
		case standing.userID != "" && standing.userID == own.OwnerID:
			verdicts[info.Name] = menuOwner
		default:
			ok, err := a.policyAdmits(r.Context(), mcUUID, standing, info, own.OwnerID)
			if err != nil {
				writeError(w, r, err)
				return
			}
			switch {
			case ok:
				verdicts[info.Name] = menuWake
			case info.AutostartPolicy == string(v1alpha1.AutostartAllowlist):
				verdicts[info.Name] = menuAllowlist
			default:
				verdicts[info.Name] = menuOwnerOnly
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": verdicts})
}

// authorizeWakeByUUID uses linked account roles for staff bypass; unlinked UUIDs have no owner or staff standing.
func (a *API) authorizeWakeByUUID(ctx context.Context, mcUUID string, info *ServerInfo, rec *ServerRecord) error {
	if info.AutostartPolicy == string(v1alpha1.AutostartPublic) {
		return nil
	}
	s, err := a.standingByUUID(ctx, mcUUID)
	if err != nil {
		return err
	}
	owner := ""
	if rec != nil {
		owner = rec.OwnerID
	}
	ok, err := a.policyAdmits(ctx, mcUUID, s, info, owner)
	if err != nil {
		return err
	}
	if !ok {
		return errForbidden
	}
	return nil
}

// uuidStanding has an empty user ID for unlinked UUIDs.
type uuidStanding struct {
	userID string
	staff  bool
}

// standingByUUID treats a missing link as no standing and a missing user as non-staff.
func (a *API) standingByUUID(ctx context.Context, mcUUID string) (uuidStanding, error) {
	userID, err := a.Repo.UserByMCUUID(ctx, mcUUID)
	switch {
	case errors.Is(err, ErrNotFound):
		return uuidStanding{}, nil
	case err != nil:
		return uuidStanding{}, err
	}
	switch u, err := a.Repo.UserByID(ctx, userID); {
	case err == nil:
		return uuidStanding{userID: userID, staff: staffRole(u.Role)}, nil
	case errors.Is(err, ErrNotFound):
		return uuidStanding{userID: userID}, nil
	default:
		return uuidStanding{}, err
	}
}

func (a *API) policyAdmits(ctx context.Context, mcUUID string, s uuidStanding, info *ServerInfo, ownerID string) (bool, error) {
	if info.AutostartPolicy == string(v1alpha1.AutostartPublic) || s.staff {
		return true, nil
	}
	if s.userID != "" && s.userID == ownerID {
		return true, nil
	}
	if info.AutostartPolicy == string(v1alpha1.AutostartAllowlist) {
		return a.Repo.UUIDInAllowlist(ctx, info.Name, mcUUID)
	}
	return false, nil
}

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
