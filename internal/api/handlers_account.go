package api

import (
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Account-linking endpoints (spec §10). The flow is forced by the
// account_link_codes schema, which carries mc_uuid but no user_id:
//
//	游戏内 /link → 生成一次性码 (internal: the in-game side has the verified UUID)
//	  → 玩家拿码 → 网页 verify 填码 (external: the web side has the logged-in user)
//	  → 写 account_links
//
// So code generation is internal-face and verification is external-face. A web
// endpoint cannot mint a code — it has no verified UUID to mint against — which
// is exactly what the schema (mc_uuid NOT NULL, no user_id) encodes. Verification
// is the load-bearing step: success there flips IsLinked true and unblocks every
// ownership operation (claim, §9.3), which otherwise dead-ends at a 412.

const (
	// linkCodeTTL bounds how long a freshly minted code is accepted (spec §10:
	// 短 TTL). Long enough to alt-tab from the game to the panel, short enough that
	// a leaked code is useless minutes later.
	linkCodeTTL = 10 * time.Minute
	// linkCodeAlphabet is a 32-symbol set with the visually ambiguous characters
	// I, O, 0 and 1 removed, so a player can read a code off chat and type it on the
	// panel without confusion. 32 divides 256 evenly, so a uniform random byte
	// reduced mod 32 is itself uniform — no modulo bias, no rejection sampling.
	linkCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// linkCodeLen is the symbol count: a 32^8 ≈ 1.1e12 keyspace, far beyond brute
	// force inside the TTL.
	linkCodeLen = 8
)

// newLinkCode returns a cryptographically random, unambiguous link code.
func newLinkCode() (string, error) {
	buf := make([]byte, linkCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = linkCodeAlphabet[int(b)%len(linkCodeAlphabet)]
	}
	return string(buf), nil
}

// createLinkCodeRequest is the in-game /link callback body (spec §10): the
// backend reports the verified UUID of the player who ran the command.
type createLinkCodeRequest struct {
	MCUUID string `json:"mc_uuid"`
}

// handleCreateLinkCode mints a one-time link code for a verified in-game UUID
// (spec §10, internal face). It is the server side of the in-game /link command:
// the backend has already established the UUID via online-mode auth, so the code
// is born bound to a trustworthy identity. The player carries the returned code
// to the panel and submits it to the external verify endpoint.
func (a *API) handleCreateLinkCode(w http.ResponseWriter, r *http.Request) {
	var req createLinkCodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.MCUUID == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "mc_uuid is required"))
		return
	}
	code, err := newLinkCode()
	if err != nil {
		writeError(w, r, err)
		return
	}
	expiresAt := a.now().Add(linkCodeTTL)
	if err := a.Repo.CreateLinkCode(r.Context(), code, req.MCUUID, expiresAt); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":       code,
		"expires_at": expiresAt.UTC(),
	})
}

// linkVerifyRequest is the panel verify-code body (spec §10): the logged-in user
// submits the code they were shown in-game.
type linkVerifyRequest struct {
	Code string `json:"code"`
}

// handleLinkVerify consumes a link code for the authenticated user and writes the
// account_links binding (spec §10, external app face). This is the load-bearing
// step of §10. The code is trimmed and uppercased so a player who typed it with
// stray spaces or in lowercase still matches the minted value. Outcomes:
// invalid/expired code → 400 invalid_code; the UUID already linked to a different
// user → 409 already_linked; otherwise the binding is written and IsLinked
// becomes true for this user.
func (a *API) handleLinkVerify(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	var req linkVerifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if code == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "code is required"))
		return
	}
	mcUUID, err := a.Repo.VerifyLinkCode(r.Context(), p.UserID, code, a.now())
	switch {
	case errors.Is(err, ErrLinkCodeInvalid):
		writeError(w, r, newError(http.StatusBadRequest, "invalid_code", "link code is invalid or expired"))
		return
	case errors.Is(err, ErrConflict):
		writeError(w, r, newError(http.StatusConflict, "already_linked",
			"that Minecraft account is already linked to another user"))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	a.audit(r, p.Email, "account.link", "")
	writeJSON(w, http.StatusOK, map[string]any{"linked": true, "mc_uuid": mcUUID})
}

// handleLinkStart reports the caller's link status and how to link (spec §10,
// external app face). It is the endpoint handleClaim's 412 points at. It
// deliberately does NOT mint a code: a code is born in-game (account_link_codes
// has no user_id column), so the web can only report status and relay the in-game
// instruction — minting here would contradict the schema. This keeps the pointer
// in handleClaim honest without pretending the web can originate a binding.
func (a *API) handleLinkStart(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	linked, err := a.Repo.IsLinked(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"linked": linked,
		"instructions": "Run /link in-game to receive a one-time code, then submit it to " +
			"POST /api/v1/account/link/verify.",
	})
}
