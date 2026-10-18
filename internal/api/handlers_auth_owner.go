package api

import (
	"log"
	"net/http"
)

// Cache only an existing owner; the probe must work before local authentication is enabled.
type ownerStatusView struct {
	OwnerBound bool `json:"owner_bound"`
}

// handleOwnerStatus returns 503 on store failure rather than reporting an unclaimed install.
func (a *API) handleOwnerStatus(w http.ResponseWriter, r *http.Request) {
	if a.ownerBound.Load() {
		writeJSON(w, http.StatusOK, ownerStatusView{OwnerBound: true})
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
	writeJSON(w, http.StatusOK, ownerStatusView{OwnerBound: bound})
}
