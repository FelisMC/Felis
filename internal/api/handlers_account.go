package api

import (
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Link codes originate from verified in-game UUIDs; the authenticated web user consumes the code to bind the account.

const (
	LinkCodeTTL = 10 * time.Minute
	// LinkCodeAlphabet omits ambiguous characters and has 32 symbols, so byte-modulo sampling is uniform.
	LinkCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	LinkCodeLen      = 8

	// authSource records the verified game authentication source at mint time and persists it when linking.
	authSourceMojang     = "mojang"
	authSourceThirdParty = "thirdparty"
)

func validAuthSource(s string) bool {
	return s == authSourceMojang || s == authSourceThirdParty
}

// errBadMCUUID maps malformed UUIDs to a client error before Postgres rejects them.
var errBadMCUUID = newError(http.StatusBadRequest, "bad_mc_uuid",
	"mc_uuid must be a UUID, such as 069a79f4-44e9-4726-a5be-fca90e38aaf5")

// parseMCUUID returns the canonical lowercase hyphenated spelling used by Postgres.
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

// deriveAuthSource recognizes Felis-nano's UUIDv3 third-party rewrite; other UUIDs default to Mojang.
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

type createLinkCodeRequest struct {
	MCUUID     string `json:"mc_uuid"`
	AuthSource string `json:"auth_source"`
}

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
	// Infer omitted sources, but reject unrecognized explicit values.
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
	resp := map[string]any{
		"code":       code,
		"expires_at": expiresAt.UTC(),
	}
	if u := a.panelURL(); u != "" {
		resp["panel_url"] = u
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleLinkStatus polls the durable UUID binding without consuming a code or returning account details.
func (a *API) handleLinkStatus(w http.ResponseWriter, r *http.Request) {
	mcUUID, err := parseMCUUID(r.PathValue("mc_uuid"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	_, err = a.Repo.UserByMCUUID(r.Context(), mcUUID)
	switch {
	case errors.Is(err, ErrNotFound):
		// Unknown and unlinked UUIDs both mean the player cannot be admitted yet.
		writeJSON(w, http.StatusOK, map[string]any{"linked": false})
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"linked": true})
}

type linkVerifyRequest struct {
	Code string `json:"code"`
}

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

// handleLinkStart reports status only; the web cannot mint codes for an unverified game UUID.
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
