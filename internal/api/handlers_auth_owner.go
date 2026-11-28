package api

import (
	"log"
	"net/http"
)

// Cache only an existing owner; the probe must work before local authentication is enabled.
type ownerStatusView struct {
	OwnerBound bool   `json:"owner_bound"`
	Mode       string `json:"mode,omitempty"`
}

// handleOwnerStatus returns 503 on store failure rather than reporting an unclaimed install.
func (a *API) handleOwnerStatus(w http.ResponseWriter, r *http.Request) {
	mode := ""
	if !a.Nano {
		m, err := a.effectiveMode(r.Context())
		if err != nil {
			log.Printf("owner status mode: %v", err)
			writeError(w, r, errAuthUnavailable)
			return
		}
		mode = m
	}
	if a.ownerBound.Load() {
		writeJSON(w, http.StatusOK, ownerStatusView{OwnerBound: true, Mode: mode})
		return
	}
	bound, err := a.Repo.AdminExists(r.Context())
	if err != nil {
		log.Printf("owner status: %v", err)
		writeError(w, r, errAuthUnavailable)
		return
	}
	if bound {
		a.ownerBound.Store(true)
	}
	writeJSON(w, http.StatusOK, ownerStatusView{OwnerBound: bound, Mode: mode})
}
