package api

import (
	"errors"
	"net/http"
	"strings"
)

// Pre-session identifier-first discovery (spec §B, #71). Given a typed email, this
// Public door reports which console login methods the account can use, so the SPA's
// identifier-first form can prompt for the right authenticator (a passkey assertion,
// or "we'll email you a code") instead of guessing.
//
// It is the deliberate counter-slice to the anti-enumeration login doors
// (handlers_auth_email.go, handlers_passkey.go): those refuse to disclose whether an
// address has an account precisely because THIS endpoint is the one sanctioned place
// existence is revealed. An empty methods array means "no (verified) account". That
// makes it a mass-enumeration surface by design — an accepted product decision, the
// same one the email door's header records. It is bounded only at the edge: the
// handler sends no mail and mutates nothing, so a per-recipient cooldown would merely
// block a legitimate retry, and per-source (client-IP) limiting is the edge's job
// (behind Cloudflare RemoteAddr is the proxy, and CGNAT would false-positive) — see
// the handlers_auth_email.go header for the same reasoning.
//
// It never reveals STAFFNESS. Methods are computed by the SAME rule for every resolved
// account — no role branch, no operator hint — so a staff email and a player email in
// the same credential state return byte-identical bodies. The console doors' own
// post-redemption staff refusal is not previewed here: a staff caller is told
// email_otp is available and is turned away only later, at op.console's Zero-Trust
// gate. Staffness is thus invisible by construction, with no side channel to regress.
type authOptionsRequest struct {
	Email string `json:"email"`
}

// handleAuthOptions resolves the typed email and returns the console login methods it
// can use. Public, pre-session, gated on local_auth_enabled like its sibling doors.
func (a *API) handleAuthOptions(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req authOptionsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	email := strings.TrimSpace(req.Email)
	if !looksLikeEmail(email) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "a valid email is required"))
		return
	}

	// methods is initialised non-nil so the no-account branch marshals as [] (not null).
	methods := []string{}

	u, err := a.Repo.UserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, ErrNotFound):
		// The sanctioned existence oracle: an unknown (or not-yet-verified) address is
		// not disguised — it honestly reports no methods.
		writeJSON(w, http.StatusOK, map[string]any{"methods": methods})
		return
	case err != nil:
		writeError(w, r, err)
		return
	}

	// Compute methods identically for EVERY resolved account. There is deliberately no
	// branch on u.Role: a staff address must be indistinguishable from a player address
	// in the same credential state, so the response carries nothing account-identifying.
	//
	// Advertise passkey only when a verifier is actually wired: both login halves 503
	// passkey_unavailable when a.Passkey is nil regardless of enrolled credentials, so
	// options must not offer a method the finish door would immediately reject.
	if a.Passkey != nil {
		creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), u.ID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if len(creds) > 0 {
			methods = append(methods, "passkey")
		}
	}
	// Email-OTP login works for any resolved verified account (UserByEmail resolves only
	// email_verified rows), so it is always on offer.
	methods = append(methods, "email_otp")

	writeJSON(w, http.StatusOK, map[string]any{"methods": methods})
}
