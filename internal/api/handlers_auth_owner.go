package api

import (
	"log"
	"net/http"
)

// Pre-session install-state probe. Until `felis setup` binds an Owner, local sign-in is
// off and every login door answers 403 local_auth_disabled, so the sign-in page would
// offer four doors that all fail. This Public route lets the page say instead that no
// Owner exists yet and how to bind one.
//
// It discloses one bit: whether the install is still unclaimed. Claiming it needs root
// on the host (`felis setup` or the break-glass console) plus a Minecraft join whose
// link code is typed into that terminal; no web door works before then, so knowing the
// bit gives a remote caller nothing to act on. It must answer while local auth is off,
// so unlike its sibling doors it is not gated on local_auth_enabled.
//
// The first true is cached in API.ownerBound. An Owner is never unbound through the
// product, so from then on the probe costs no query; before it, each call is one
// indexed LIMIT 1 read. It is not an AuthDoor: the page polls it on every load, and
// sharing the doors' per-address bucket would throttle the sign-in that follows.
type ownerStatusView struct {
	OwnerBound bool `json:"owner_bound"`
}

// handleOwnerStatus reports whether any staff account exists. A store failure is a 503,
// so the page falls back to its normal doors rather than claiming the install is unbound.
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
