package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// Username-collision reclaim (spec §B3, internal face). The configured
// third-party Yggdrasil and the official Mojang service can mint the SAME
// username under DIFFERENT UUIDs. Velocity detects the collision in a limbo login
// server; when the connecting player is NOT the genuine Mojang owner, Mojang
// takes priority (正版优先): the squatter is rejected and barred, and its data is
// stashed for a 30-day window so a new account can inherit it.
//
// These two endpoints are the Go-verifiable data layer of that flow, both
// internal-face (velocity holds a service token, never a web Principal):
//
//	POST /api/v1/internal/player/reclaim          — record a reclaim (bar + stash)
//	GET  /api/v1/internal/player/blacklist/{uuid} — the login gate's bar check
//
// Velocity collision-routing, the limbo prompt, the authlib dual-backend, and the
// data-inherit flow are CODE-ONLY (Java + a QR-bound device session a Postgres
// row cannot express) and are not represented here. The block is keyed by UUID,
// never by the contested name, so the genuine Mojang player — same username,
// different UUID — is never caught.
//
// This UUID-keyed, proxy-detected split matches the real multi-Yggdrasil reference
// (CaaMoe/MultiLogin binds identity in the plugin as serviceId+online-UUID, keyed by
// UUID, never by name). §B3's Mojang-priority reclaim goes beyond the common "protect
// the first-bound name" behavior: it evicts a squatter once the genuine Mojang owner
// appears and stashes the squatter's data for the code-only inherit path above.

const (
	// reclaimHoldTTL is the 30-day window a reclaimed account's data is stashed for
	// before it may be purged (spec §B3 "您的数据将会被暂存 30 天"). The hold's
	// expires_at is the API clock + this, so one authoritative clock drives expiry.
	reclaimHoldTTL = 30 * 24 * time.Hour
)

// newHoldID returns an opaque random row id (128 bits, hex) for a
// player_data_holds row, mirroring newOTPID.
func newHoldID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// reclaimRequest is the velocity reclaim callback body (spec §B3): the verified
// online-mode UUID of the squatter being displaced, the contested username (for
// display/audit), and an optional opaque handle to the data already archived for
// the hold. data_ref is optional — archival may be deferred — but a squatter_uuid
// and username are always required.
type reclaimRequest struct {
	SquatterUUID string `json:"squatter_uuid"`
	Username     string `json:"username"`
	DataRef      string `json:"data_ref"`
}

// handleReclaimUsername records a Mojang-priority username reclaim (spec §B3,
// internal face). In one transaction it bars the squatter UUID and stashes its
// data as a 30-day hold (Repo.ReclaimUsername). It is idempotent: a repeat
// reclaim of an already-barred UUID is a no-op that still answers 200, so a
// retried velocity callback is harmless. It returns the hold's expiry so velocity
// can tell the rejected player how long their data is kept.
func (a *API) handleReclaimUsername(w http.ResponseWriter, r *http.Request) {
	var req reclaimRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.SquatterUUID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "squatter_uuid is required"))
		return
	}
	if req.Username == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "username is required"))
		return
	}
	// Admin-on-Yggdrasil exception (spec §B3). Before barring the holder, check
	// whether the displaced UUID is a Linked Operator/SysAdmin authenticating through
	// the third-party Yggdrasil. Such a holder is staff on the Login Server, not a
	// Mojang squatter, so Mojang priority must NOT displace them: refuse the reclaim
	// outright — no bar, no stash — so the protected admin never enters the blacklist
	// and the login gate naturally passes them. The exception is scoped strictly to
	// admins; an ordinary thirdparty player is still reclaimed (Mojang priority holds).
	protected, err := a.Repo.IsProtectedAdminLink(r.Context(), req.SquatterUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if protected {
		// Distinct audit action so a refusal is never mistaken for a bar — the
		// accountability record shows the reclaim was declined, and why.
		payload, _ := json.Marshal(map[string]string{
			"username": req.Username, "squatter_uuid": req.SquatterUUID, "reason": "protected_admin"})
		a.auditEntry(r, AuditEntry{
			Actor: "velocity", Source: internalSource(r), Action: "player.reclaim.refused", Payload: payload,
		})
		writeError(w, r, newError(http.StatusConflict, "protected_admin",
			"that username belongs to a linked administrator on the login server and cannot be reclaimed"))
		return
	}
	id, err := newHoldID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	proposedExpiry := a.now().Add(reclaimHoldTTL)
	// ReclaimUsername returns the EFFECTIVE persisted expiry, which differs from the
	// proposed one on an idempotent retry (the hold keeps its first window). We echo
	// the persisted value so a re-firing velocity callback never tells the player a
	// 30-day window that the stored hold does not actually have.
	heldUntil, err := a.Repo.ReclaimUsername(r.Context(), id, req.SquatterUUID, req.Username, req.DataRef, proposedExpiry)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// A reclaim bars a player and stashes their world — a security-significant
	// accountability event. The squatter UUID and contested name go in the audit
	// payload (the flat columns model a server op, not this), keyed by Source
	// internal since velocity, not a human, drives it.
	payload, _ := json.Marshal(map[string]string{"username": req.Username, "squatter_uuid": req.SquatterUUID})
	a.auditEntry(r, AuditEntry{
		Actor: "velocity", Source: internalSource(r), Action: "player.reclaim", Payload: payload,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"blacklisted":     true,
		"username":        req.Username,
		"hold_expires_at": heldUntil.UTC(),
	})
}

// handleCheckBlacklist reports whether an in-game UUID was barred by a prior
// reclaim (spec §B3, internal face). The velocity login gate calls it to reject a
// squatter before letting them in; the genuine Mojang UUID (same name, different
// UUID) is never on the list, so it always passes.
func (a *API) handleCheckBlacklist(w http.ResponseWriter, r *http.Request) {
	mcUUID, err := parseMCUUID(r.PathValue("mc_uuid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	blacklisted, err := a.Repo.IsUsernameBlacklisted(r.Context(), mcUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blacklisted": blacklisted})
}
