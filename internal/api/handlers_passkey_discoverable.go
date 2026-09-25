package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Discoverable ("usernameless") passkey login (spec §14, task #40) — the TRULY from-zero
// console.<root_domain> door. Its email-first sibling (handlers_passkey.go) still needs a typed
// email to resolve the account before offering its passkeys; this door needs nothing typed at
// all. The browser calls navigator.credentials.get() with an EMPTY allowCredentials, the
// authenticator offers a resident credential it holds for this RP, and the account is revealed
// only by the userHandle inside the signed assertion. Because there is no identifier at begin,
// the challenge cannot be user-keyed: it is stashed under an opaque server-minted handle
// (login_id) in the non-user-keyed store (migration 0013) and echoed back at finish. Email-OTP
// and username-first passkey remain the fallbacks, so an authenticator that stored no resident
// key is never locked out — only its from-zero convenience is unavailable.
//
// Anti-abuse: a usernameless begin has no recipient OR principal to key a limit on, so one client
// is bounded by the per-address token bucket every public auth door sits behind
// (throttleAuthDoor), each network (IPv4 host or IPv6 /48, challengeSource) by
// maxLiveChallengesPerSource live challenges, and the table by a global cap on live challenges;
// CreateDiscoverableChallenge enforces both bounds atomically (ErrTooManyPasskeyChallenges →
// 429). A flood from one network fills its own allowance and leaves every other network its
// sign-ins.

// errTooManyChallenges answers a passkey login begin over a challenge bound.
var errTooManyChallenges = newError(http.StatusTooManyRequests, "too_many_challenges",
	"too many passkey logins in progress from this network; try again in a few minutes")

// handlePasskeyLoginDiscoverableBegin starts a usernameless assertion ceremony (Public,
// pre-session). It has no request body — the whole point is that the caller supplies no
// identifier — but requires the JSON Content-Type as the same cross-origin CSRF guard the other
// pre-session doors use. It asks the verifier for assertion options with an empty
// allowCredentials + opaque SessionData, stashes the SessionData under a fresh opaque handle in
// the capped non-user-keyed store, and returns the options with that handle merged in as
// login_id for the browser to echo at finish.
func (a *API) handlePasskeyLoginDiscoverableBegin(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	options, sessionData, err := a.Passkey.BeginDiscoverableLogin()
	if err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_failed",
			"could not start passkey login"))
		return
	}
	id, err := newPasskeyID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	now := a.now()
	if err := a.Repo.CreateDiscoverableChallenge(r.Context(), id, challengeSource(a.clientIP(r)), sessionData, now, now.Add(passkeyChallengeTTL)); err != nil {
		if errors.Is(err, ErrTooManyPasskeyChallenges) {
			writeError(w, r, errTooManyChallenges)
			return
		}
		writeError(w, r, err)
		return
	}
	// Merge the opaque login handle into the options envelope so the response is a single
	// {"publicKey": {...}, "login_id": "..."} document. The browser passes publicKey to
	// navigator.credentials.get() and echoes login_id back at finish (the challenge is never
	// user-keyed, so this handle is the only link between begin and finish).
	envelope, err := mergeLoginID(options, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, envelope)
}

// passkeyDiscoverableFinishRequest is the finish body: the opaque login_id that begin returned
// (the only link to the stashed challenge, since it is not user-keyed) and the raw
// navigator.credentials.get() assertion. Assertion is RawMessage so the exact bytes the browser
// produced reach the verifier without a re-encode that could perturb the signed payload.
type passkeyDiscoverableFinishRequest struct {
	LoginID   string          `json:"login_id"`
	Assertion json.RawMessage `json:"assertion"`
}

// handlePasskeyLoginDiscoverableFinish verifies a usernameless assertion and mints a session
// (Public, pre-session). It consumes the stashed challenge under login_id (a missing/expired/
// consumed handle → 400), then verifies the assertion — the verifier resolves the account from
// the authenticator-revealed userHandle via the resolve callback below, WITHOUT any
// client-supplied identifier. On success the session is minted for the account the assertion
// actually resolved AND verified to (the resolved user is hoisted out of the callback), never
// anything the client named. All rejection branches collapse to one passkey_login_invalid
// envelope so finish is never an existence/state oracle.
func (a *API) handlePasskeyLoginDiscoverableFinish(w http.ResponseWriter, r *http.Request) {
	if !localAuthEnabled(r.Context(), a.Repo) {
		writeError(w, r, newError(http.StatusForbidden, "local_auth_disabled",
			"session login is disabled"))
		return
	}
	if a.Passkey == nil {
		writeError(w, r, errPasskeyUnavailable)
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req passkeyDiscoverableFinishRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.LoginID) == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "login_id is required"))
		return
	}
	if len(req.Assertion) == 0 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "assertion is required"))
		return
	}

	sessionData, err := a.Repo.ConsumeDiscoverableChallenge(r.Context(), req.LoginID, a.now())
	if err != nil {
		if errors.Is(err, ErrPasskeyChallengeInvalid) {
			a.authFailure(r, "passkey_discoverable", "challenge_invalid", nil)
			writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
				"passkey login could not be completed; begin again"))
			return
		}
		writeError(w, r, err)
		return
	}

	// The verifier hands the authenticator-revealed userHandle to this resolver; it loads the
	// account and its bound credentials so ValidateDiscoverableLogin can check the asserted
	// credential belongs to that user and verify the signature. The userHandle IS the account's
	// stable id (WebAuthnID), so this is a direct id lookup. The resolved user is hoisted here so
	// the session below is minted for the account the assertion actually resolved AND verified to
	// — not anything the client supplied (the body carries only a challenge handle).
	var resolved *StaffUser
	var resolvedCreds []PasskeyCredential
	resolve := func(userHandle []byte) (PasskeyUser, error) {
		u, err := a.Repo.UserByID(r.Context(), string(userHandle))
		if err != nil {
			return PasskeyUser{}, err
		}
		// A disabled or soft-deleted account must not complete a login even when it
		// still holds a credential (UserByID is an unfiltered lookup shared with admin
		// reads, so the liveness check lives here, at the door). Fail closed with the
		// same opaque outcome as an unknown handle (audit #33).
		if d, err := a.Repo.UserDetail(r.Context(), u.ID); err != nil || d.Disabled || d.DeletedAt != nil {
			return PasskeyUser{}, ErrNotFound
		}
		creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), u.ID)
		if err != nil {
			return PasskeyUser{}, err
		}
		resolved, resolvedCreds = u, creds
		// Name/DisplayName are cosmetic at assertion time (nothing is shown to the user); use the
		// stable username so a nil email never matters.
		return PasskeyUser{ID: u.ID, Name: u.Username, DisplayName: u.Username, Credentials: creds}, nil
	}
	va, err := a.Passkey.FinishDiscoverableLogin(resolve, sessionData, bytes.NewReader(req.Assertion))
	if err != nil {
		// resolved is set when the credential named a live account and only the
		// signature (or the credential's binding) failed.
		a.authFailure(r, "passkey_discoverable", "bad_assertion", resolved)
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
			"passkey login could not be completed; begin again"))
		return
	}
	// A verified assertion guarantees resolve ran and set resolved: go-webauthn calls the handler
	// to obtain the user BEFORE checking the signature, and a resolve error would have failed
	// FinishDiscoverableLogin above. Guard anyway so a future verifier that could return success
	// without invoking the resolver fails closed rather than nil-dereferencing.
	if resolved == nil {
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
			"passkey login could not be completed; begin again"))
		return
	}
	// Same UV + clone policy + counter advance as the username-first door (applyAssertion): a
	// refusal gets the identical opaque envelope but is audited under the resolved account; a
	// successful assertion advances the stored counter and stamps last_used_at.
	if err := a.applyAssertion(r.Context(), va, resolvedCreds); err != nil {
		if a.passkeyAssertionRejected(r, "passkey_discoverable", resolved, va.CredentialID, err) {
			writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
				"passkey login could not be completed; begin again"))
			return
		}
		writeError(w, r, err)
		return
	}

	if err := a.startSession(w, r, resolved.ID, provenSignIn); err != nil {
		writeError(w, r, err)
		return
	}
	a.auditAccount(r, resolved, "auth.passkey_login_discoverable", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": resolved.ID,
		"role":    resolved.Role,
	})
}

// mergeLoginID returns options with an added top-level "login_id" member, so a discoverable
// begin can hand the browser one {"publicKey": {...}, "login_id": "..."} document. It parses the
// options into a generic envelope (they are already a JSON object with a publicKey member) and
// re-marshals with the handle added; a malformed options blob surfaces as an error rather than a
// silently unmergeable response.
func mergeLoginID(options json.RawMessage, id string) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(options, &envelope); err != nil {
		return nil, err
	}
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	envelope["login_id"] = idJSON
	return json.Marshal(envelope)
}
