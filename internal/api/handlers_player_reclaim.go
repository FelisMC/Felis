package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// Reclaim bans the displaced UUID rather than the contested name, preserving access for the distinct Mojang identity.

const (
	reclaimHoldTTL = 30 * 24 * time.Hour
)

func newHoldID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type reclaimRequest struct {
	SquatterUUID string `json:"squatter_uuid"`
	Username     string `json:"username"`
	DataRef      string `json:"data_ref"`
}

// handleReclaimUsername atomically bans and holds data; repeat requests preserve the original hold and return 200.
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
	// Linked third-party staff identities are exempt from Mojang-priority displacement.
	protected, err := a.Repo.IsProtectedAdminLink(r.Context(), req.SquatterUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if protected {
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
	// Echo the persisted expiry so retries cannot advertise a longer hold than the original request created.
	heldUntil, err := a.Repo.ReclaimUsername(r.Context(), id, req.SquatterUUID, req.Username, req.DataRef, proposedExpiry)
	if err != nil {
		writeError(w, r, err)
		return
	}
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
