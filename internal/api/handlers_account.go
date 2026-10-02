package api

import (
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
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
	// LinkCodeTTL bounds how long a freshly minted code is accepted (spec §10:
	// 短 TTL). Long enough to alt-tab from the game to the panel, short enough that
	// a leaked code is useless minutes later.
	LinkCodeTTL = 10 * time.Minute
	// LinkCodeAlphabet is a 32-symbol set with the visually ambiguous characters
	// I, O, 0 and 1 removed, so a player can read a code off chat and type it on the
	// panel without confusion. 32 divides 256 evenly, so a uniform random byte
	// reduced mod 32 is itself uniform — no modulo bias, no rejection sampling.
	LinkCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// LinkCodeLen is the symbol count: a 32^8 ≈ 1.1e12 keyspace, far beyond brute
	// force inside the TTL.
	LinkCodeLen = 8

	// authSource records which Yggdrasil established the in-game UUID when a code
	// was minted (spec §10, dual-Yggdrasil): the official Mojang service, or a
	// configured thirdparty. It is captured at mint (the only place that knows it)
	// and copied onto the durable link at verify; the web side never sees the
	// authentication. These mirror the link_auth_source enum (migration 0005).
	authSourceMojang     = "mojang"
	authSourceThirdParty = "thirdparty"
)

// validAuthSource reports whether s is a recognised link_auth_source value. An
// empty string is NOT valid here — handleCreateLinkCode defaults it before this
// check, so a non-empty value reaching validation must be one we can store.
func validAuthSource(s string) bool {
	return s == authSourceMojang || s == authSourceThirdParty
}

// errBadMCUUID answers an mc_uuid that is not a UUID. Every mc_uuid column is
// Postgres's uuid type, which refuses such text with 22P02, and that reached the
// caller as a 500.
var errBadMCUUID = newError(http.StatusBadRequest, "bad_mc_uuid",
	"mc_uuid must be a UUID, such as 069a79f4-44e9-4726-a5be-fca90e38aaf5")

// parseMCUUID reads an mc_uuid from a request: surrounding spaces trimmed, empty
// → 400 bad_request "mc_uuid is required", not a UUID → errBadMCUUID. It returns
// the canonical lowercase hyphenated form, the text Postgres gives back for the
// column, so what a handler stores, echoes and compares is one spelling.
func parseMCUUID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", newError(http.StatusBadRequest, "bad_request", "mc_uuid is required")
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return "", errBadMCUUID
	}
	return id.String(), nil
}

// deriveAuthSource infers the auth source from the UUID's version nibble when
// the minting backend omitted auth_source. Felis-nano rewrites every
// third-party profile to a name-based UUIDv3 under its namespace before it ever
// reaches the proxy, while Mojang profiles keep their random v4 — so on a
// nano-fronted deployment the version nibble alone identifies the source, and
// no Java plugin has to learn the field. Anything unparseable keeps the
// historical Mojang-priority default.
func deriveAuthSource(mcUUID string) string {
	hex := strings.ReplaceAll(mcUUID, "-", "")
	if len(hex) != 32 {
		return authSourceMojang
	}
	switch hex[12] {
	case '3':
		return authSourceThirdParty
	default:
		return authSourceMojang
	}
}

// newLinkCode returns a cryptographically random, unambiguous link code.
func newLinkCode() (string, error) {
	buf := make([]byte, LinkCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = LinkCodeAlphabet[int(b)%len(LinkCodeAlphabet)]
	}
	return string(buf), nil
}

// createLinkCodeRequest is the in-game /link callback body (spec §10): the
// backend reports the verified UUID of the player who ran the command, plus how
// that UUID was authenticated (auth_source). auth_source is optional — an older
// backend that omits it falls back to the Mojang-priority default — but a value
// that IS sent must be one we can store.
type createLinkCodeRequest struct {
	MCUUID     string `json:"mc_uuid"`
	AuthSource string `json:"auth_source"`
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
	mcUUID, err := parseMCUUID(req.MCUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Default an omitted source from the UUID's version nibble (v3 = felis-nano
	// third-party rewrite, v4 = Mojang; see deriveAuthSource) but reject an
	// unrecognised explicit one — a typo'd source must not silently land as a
	// stored value the panel will later mislabel.
	authSource := req.AuthSource
	if authSource == "" {
		authSource = deriveAuthSource(mcUUID)
	}
	if !validAuthSource(authSource) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"auth_source must be %q or %q", authSourceMojang, authSourceThirdParty))
		return
	}
	code, err := newLinkCode()
	if err != nil {
		writeError(w, r, err)
		return
	}
	expiresAt := a.now().Add(LinkCodeTTL)
	if err := a.Repo.CreateLinkCode(r.Context(), code, mcUUID, authSource, expiresAt); err != nil {
		writeError(w, r, err)
		return
	}
	// panel_url tells the in-game side where the player redeems the code, so
	// every plugin renders the same address from one source of truth instead of
	// each baking in its own hostname. Omitted when no hostname is configured.
	resp := map[string]any{
		"code":       code,
		"expires_at": expiresAt.UTC(),
	}
	if u := a.panelURL(); u != "" {
		resp["panel_url"] = u
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleLinkStatus reports whether an in-game UUID has finished linking yet — the
// completion poll of the QR scan-to-login flow (spec §B3 player game-login; memory
// player-login-yggdrasil). It is internal-face and read-only, the device-code
// "poll for completion" step that turns the typed-code link into a scan:
//
//	new player joins → velocity mints a code (handleCreateLinkCode) and renders it
//	  as a QR → player scans it on a phone already signed in to console.<root_domain>
//	  → that web session's verify (handleLinkVerify) writes the durable account_links
//	  row bound to THAT user → velocity polls HERE for the same UUID it minted against
//	  → on {linked:true} it admits the player with no reconnect — the whole point of
//	  scanning over typing. The response is deliberately just the boolean: the plugin
//	  keys everything on the UUID it already holds, so no identity detail crosses back.
//
// The poll is keyed by the verified mc_uuid velocity already holds, not by the
// scanned code, so it is a pure idempotent read of the durable link (UserByMCUUID):
// there is no transient device-session row, nothing is consumed, and a velocity
// restart re-polls safely. The secret is the short-TTL code the player scans, never
// this public UUID, so the read carries no guessing surface and needs no attempt
// cap — the internal face already gates it to service callers.
//
// CODE-ONLY (Java/Velocity, not represented here): rendering the code as a QR, the
// limbo collision routing, and admitting the polled player into the main server.
// KNOWN-LIMITATION: the reclaim disambiguation a scan can surface — "start fresh"
// vs "inherit the 30-day-held data" — is the data-inherit choice that
// handlers_player_reclaim.go keeps CODE-ONLY (reclaimed_by_user_id stays NULL on
// the verifiable path); this endpoint reports link completion only, not that choice.
func (a *API) handleLinkStatus(w http.ResponseWriter, r *http.Request) {
	mcUUID, err := parseMCUUID(r.PathValue("mc_uuid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	_, err = a.Repo.UserByMCUUID(r.Context(), mcUUID)
	switch {
	case errors.Is(err, ErrNotFound):
		// Not linked yet. For the poller this is simply "keep waiting": velocity
		// polls until its own code TTL lapses. A never-seen UUID is indistinguishable
		// from a not-yet-scanned one, and deliberately so — both mean "do not admit".
		writeJSON(w, http.StatusOK, map[string]any{"linked": false})
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"linked": true})
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
	mcUUID, authSource, err := a.Repo.VerifyLinkCode(r.Context(), p.UserID, code, a.now())
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
	a.audit(r, "account.link", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"linked": true, "mc_uuid": mcUUID, "auth_source": authSource,
	})
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
