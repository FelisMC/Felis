package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Discoverable login stores challenges by opaque login_id because no account is known until the assertion resolves it.

var errTooManyChallenges = newError(http.StatusTooManyRequests, "too_many_challenges",
	"too many passkey logins in progress from this network; try again in a few minutes")

// handlePasskeyLoginDiscoverableBegin requires JSON Content-Type for CSRF protection, despite having no request body.
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
	// Return login_id alongside publicKey so finish can locate the challenge without a user identifier.
	envelope, err := mergeLoginID(options, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, envelope)
}

type passkeyDiscoverableFinishRequest struct {
	LoginID   string          `json:"login_id"`
	Assertion json.RawMessage `json:"assertion"`
}

// handlePasskeyLoginDiscoverableFinish mints a session for the verified userHandle and returns uniform rejection responses.
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

	// Resolve the stable userHandle to its account and credentials before verifying the assertion signature.
	var resolved *StaffUser
	var resolvedCreds []PasskeyCredential
	resolve := func(userHandle []byte) (PasskeyUser, error) {
		u, err := a.Repo.UserByID(r.Context(), string(userHandle))
		if err != nil {
			return PasskeyUser{}, err
		}
		// UserByID is unfiltered; reject disabled or deleted accounts with the same opaque failure.
		if d, err := a.Repo.UserDetail(r.Context(), u.ID); err != nil || d.Disabled || d.DeletedAt != nil {
			return PasskeyUser{}, ErrNotFound
		}
		creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), u.ID)
		if err != nil {
			return PasskeyUser{}, err
		}
		resolved, resolvedCreds = u, creds
		// Name and DisplayName are cosmetic during assertion; a verified email is unnecessary.
		return PasskeyUser{ID: u.ID, Name: u.Username, DisplayName: u.Username, Credentials: creds}, nil
	}
	va, err := a.Passkey.FinishDiscoverableLogin(resolve, sessionData, bytes.NewReader(req.Assertion))
	if err != nil {
		// Retain the live account for audit even if credential binding or signature verification fails.
		a.authFailure(r, "passkey_discoverable", "bad_assertion", resolved)
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
			"passkey login could not be completed; begin again"))
		return
	}
	if resolved == nil {
		writeError(w, r, newError(http.StatusBadRequest, "passkey_login_invalid",
			"passkey login could not be completed; begin again"))
		return
	}
	// Apply the same user-verification, clone and counter policy as email-first login.
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
