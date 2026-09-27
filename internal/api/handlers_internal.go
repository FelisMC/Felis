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
	// A server that is up and meant to stay up has nothing to wake, and joining it
	// is open to every linked player: host routing admits them on the link check
	// alone. Putting the no-op through autostartPolicy answered a friend's menu
	// "join" with "you may not start this server". Nothing changes here, so there
	// is no cooldown to spend and nothing to audit.
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
	// Given up or being deleted: it stays down until the reaper archives it, and
	// velocity tells the player so instead of queueing them.
	if rec != nil && rec.Retire != nil {
		writeError(w, r, errServerRetiring)
		return
	}
	// A start whose automatic restarts are spent (or that can never succeed as
	// configured) stays down until a person looks at it. The 202 this used to
	// return queued the player for a server nothing was starting. The join leaves
	// the restart budget alone, or every player who tried to join would buy
	// another three crash loops; velocity tells them and queues no one. A Failed
	// server still inside its backoff is not this: its next attempt is coming, so
	// it gets the 202 and the player waits for it.
	if info.StartGaveUp && info.DesiredState == string(v1alpha1.DesiredRunning) {
		writeError(w, r, newError(http.StatusConflict, "start_failed",
			"the server failed to start and its automatic retries are spent; its owner can retry from the panel"))
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
	// restore/backup/file write finishes, so it does not enqueue the player. The
	// idle reaper gets its own code: when it lets go, the world is archived and the
	// server released, so "try again shortly" would send the player back to a server
	// that is no longer the one they knew.
	if err := a.Cluster.SetDesiredState(r.Context(), name, v1alpha1.DesiredRunning); err != nil {
		var busy *MaintenanceBusyError
		if errors.As(err, &busy) && busy.Kind == maintenance.KindReap {
			writeError(w, r, errWorldReclaiming)
			return
		}
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

// errWorldReclaiming refuses a join-driven wake while the idle reaper archives the
// server's world. Once it is done the server is released with an empty world and
// the old one stays in the archive.
var errWorldReclaiming = newError(http.StatusConflict, "world_reclaiming",
	"this server sat idle too long and its world is being archived; afterwards it is released with an empty world")

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
	mcUUID, err := parseMCUUID(req.MCUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// ① resolve identity by UUID. An unlinked UUID (no account_links row) cannot
	// establish ownership; a successful resolve already implies linked, so there is
	// no separate IsLinked check (mirrors the external claim's order, link → quota
	// → write). ErrNotFound here is "claimer not linked" (412), never "server
	// missing" — that distinction is the claim call's, below.
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

	// ② quota gate, evaluated before the ownership write (mirrors handleClaim).
	// All four dimensions (servers, CPU, memory, storage) are checked, with the
	// server at its real size.
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
// renders a sane tile rather than erroring. A server being deleted is not claimable
// even while it has no owner.
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

// Menu access verdicts: what a lobby menu click on a server that is not up would
// meet for one player (handleInternalMenuAccess).
const (
	menuRetiring    = "retiring"     // given up or being deleted: nobody starts it
	menuStartFailed = "start_failed" // automatic restarts spent: waits for its owner
	menuOwner       = "owner"        // the player's own server
	menuWake        = "wake"         // the policy lets this player start it
	menuOwnerOnly   = "owner_only"   // ownerOnly (or unset): only its owner starts it
	menuAllowlist   = "allowlist"    // allowlist, and the player is not on it
)

// handleInternalMenuAccess answers the lobby menu's per-player question (spec §12):
// for every user server, whether this verified UUID may start it and why not. The
// tiles' live state stays in the shared per-server projection (…/menu); this is one
// call per menu open, so a lobby full of players still reads each server's status
// once. A server that is up is open to every linked player (handleInternalWake), so
// the lobby shows Join there whatever the verdict. The verdicts follow the wake's
// own gates with the transient refusals (cooldown, the running-server cap) left out,
// since a retry gets past those. Retiring comes first: nobody may start such a
// server, so it is the reason a stranger is shown too.
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
			continue // the login gate and the lobby are not menu tiles
		}
		own := owners[info.Name]
		switch {
		case own.Retire != nil:
			verdicts[info.Name] = menuRetiring
		case info.StartGaveUp && info.DesiredState == string(v1alpha1.DesiredRunning):
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

// uuidStanding is what a verified in-game UUID brings to the autostartPolicy gate:
// the user it is linked to ("" when unlinked) and whether that user is staff.
type uuidStanding struct {
	userID string
	staff  bool
}

// standingByUUID resolves the UUID to its linked user once. A missing link is not
// an error here — it just means "no standing", and a link pointing at a vanished
// user reads as the link without the staff role.
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

// policyAdmits is the autostartPolicy gate itself: public admits anyone, staff and
// the owner (ownerID, "" while unclaimed) pass every policy, allowlist admits a
// listed UUID, and ownerOnly or unset admits no one else.
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
