package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Player-console onboarding bootstrap (console-tier access model; spec §10, §B). This
// is the ONE public, pre-account entrypoint of the player console (console.<root_domain>):
// an account-less player redeems the one-time Bind Code they generated in the in-game
// Login Lobby, and in a single step the platform creates their player account
// (role=user), binds it to their verified in-game UUID, and mints a player session.
// From there the ordinary app-tier onboarding endpoints (email-OTP, passkey) work off
// the resulting principal like any other — the session model is role-agnostic, so a
// player session is just an opaque felis_session over a role=user row.
//
// Why this may be Public while /account/link/verify may not: a Bind Code is minted
// INTERNAL-face only (handleCreateLinkCode), with a short TTL, single use, and a 32^8
// keyspace, so the web can never originate one (account_link_codes has no user_id) —
// no code, no account. This endpoint MATERIALLY elevates the code's authority: where
// /account/link/verify bound a UUID to an already-authenticated principal, this makes
// the code alone create an account and mint a session. That is only safe if the code
// was minted against a UUID an online-mode Yggdrasil actually authenticated — a
// precondition that lives in velocity/Java (CODE-ONLY, not verifiable from this repo).
// So the safety here is INTEGRATION-dependent on that upstream online-mode guarantee;
// the Go layer proves only the account/session logic (role, refuse-staff, idempotent),
// never the identity guarantee itself.
//
// No app-level attempt cap is enforced here (unlike the email-OTP flow, whose 1e6
// keyspace demanded one): the code's ~1e12 keyspace, single use and short TTL make
// blind brute force non-viable, and rate-limiting is deferred to the edge exactly as
// for the other public session doors (email-OTP, op-login). The idempotent returning-player branch (a UUID already
// linked to a role=user player is fetched, not re-created) is a DELIBERATE standing
// "log in via the game" door, not merely first-time onboarding: control of the
// in-game identity is the root of trust, so re-minting a code always re-grants a
// session even after email/passkey are bound. "登录并非强制，但没登录什么都干不了".
//
// op.console stays behind Zero Trust. A code whose UUID belongs to STAFF (admin or
// owner) is refused here (ErrPlayerBindForbidden → 403), so the public bootstrap
// provably never mints a session for a staff identity — the sole tier it yields is a
// role=user player session, host-only to console.<root_domain> (never sent to
// op.console) and carrying ViaAdminAccess=false. "op.console 必须得 Auth".

// newUserID returns an opaque random user id (128 bits, hex), matching the shape of
// the ids break-glass mints for staff rows.
func newUserID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// bindRedeemRequest is the console bootstrap body: the Bind Code the player was shown
// in the Login Lobby.
type bindRedeemRequest struct {
	Code string `json:"code"`
}

// handleBindRedeem redeems a Bind Code into a player account + session (Public). It is
// the account-less player's only door into console.<root_domain>: no prior principal,
// no Zero Trust in front (unlike op.console). Like the email-OTP login door it is a
// cookie-minting public route, so it requires local sessions to be enabled and a JSON
// content type (the cross-site-forgery guard) and mints the same host-only
// felis_session cookie.
// The code is trimmed and uppercased so a player who typed it with stray spaces or in
// lowercase still matches, mirroring handleLinkVerify.
func (a *API) handleBindRedeem(w http.ResponseWriter, r *http.Request) {
	// The minted session is a felis_session cookie, honored only when local sessions
	// are enabled (SessionAuth). Minting one while they are off would hand back a dead
	// cookie, so refuse loudly, consistently with the other session doors. This couples the
	// player bootstrap to the same toggle that gates op.console local login; a future
	// deployment wanting player cookies without local admin login would decouple them
	// in SessionAuth — out of scope here (KNOWN coupling).
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body bindRedeemRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(body.Code))
	if code == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "code is required"))
		return
	}

	uid, err := newUserID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	userID, mcUUID, authSource, err := a.Repo.RedeemPlayerBindCode(r.Context(), uid, code, a.now())
	switch {
	case errors.Is(err, ErrLinkCodeInvalid):
		a.authFailure(r, "bind_redeem", "bad_code", nil)
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "bind code is invalid or expired"))
		return
	case errors.Is(err, ErrPlayerBindForbidden):
		a.authFailure(r, "bind_redeem", "staff_account", nil)
		writeError(w, r, newError(http.StatusForbidden, "staff_account",
			"that Minecraft account belongs to staff; sign in at the operator console"))
		return
	case errors.Is(err, ErrPlayerAccountRetired):
		a.authFailure(r, "bind_redeem", "account_retired", nil)
		// The linked Felis account is disabled or soft-deleted: the door refuses to
		// reuse it, because minting a session here would resurrect the account the
		// owner just retired (audit #33). The code survives, so re-enabling the
		// account and retrying within its TTL still works.
		writeError(w, r, newError(http.StatusForbidden, "account_retired",
			"this Minecraft account's Felis account is disabled or deleted; contact the operator"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}

	if err := a.startSession(w, r, userID, bindCodeSignIn); err != nil {
		writeError(w, r, err)
		return
	}
	// The in-game code proved the Minecraft account; it names the actor.
	a.auditEntry(r, AuditEntry{Actor: "mc:" + mcUUID, ActorUserID: userID, Action: "account.bind_redeem"})
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": userID, "linked": true, "mc_uuid": mcUUID, "auth_source": authSource,
	})
}
