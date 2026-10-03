package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

type linkedProfile struct {
	Source      string `json:"source"`
	Name        string `json:"name"`
	ProfileUUID string `json:"profile_uuid"`
	MCUUID      string `json:"mc_uuid"`
	AuthSource  string `json:"auth_source"`
}

func profileAPIBase(src AuthSource) string {
	if src.APIURL != "" {
		return strings.TrimRight(src.APIURL, "/")
	}
	const suffix = "/sessionserver/session/minecraft/hasJoined"
	if strings.HasSuffix(src.URL, suffix) {
		return strings.TrimSuffix(src.URL, suffix)
	}
	return ""
}

func (a *API) handleLinkSources(w http.ResponseWriter, r *http.Request) {
	type sourceView struct {
		Tag             string `json:"tag"`
		LookupAvailable bool   `json:"lookup_available"`
	}
	sources := make([]sourceView, 0, len(a.AuthSources))
	for _, src := range a.AuthSources {
		sources = append(sources, sourceView{src.Tag, src.Identity || profileAPIBase(src) != ""})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (a *API) handleLookupProfile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	profile, err := a.lookupProfile(r.Context(), q.Get("source"), q.Get("profile"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// handleLinkProfile is a staff designation, not proof of game-account ownership.
// The user must already have panel authority and a fresh login factor. A client
// supplies only the selected source and native role UUID; mapping and target user
// are determined on the server.
func (a *API) handleLinkProfile(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !a.requireReauth(w, r, p) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		Source      string `json:"source"`
		ProfileUUID string `json:"profile_uuid"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := uuid.Parse(req.ProfileUUID); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "profile_uuid must be a role UUID"))
		return
	}
	profile, err := a.lookupProfile(r.Context(), req.Source, req.ProfileUUID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	err = a.Repo.LinkAccount(r.Context(), p.UserID, profile.MCUUID, profile.AuthSource)
	if errors.Is(err, ErrConflict) {
		writeError(w, r, newError(http.StatusConflict, "already_linked", "that Minecraft role is linked to another user"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "account.link_profile", "")
	writeJSON(w, http.StatusOK, map[string]any{"linked": true, "mc_uuid": profile.MCUUID, "auth_source": profile.AuthSource})
}

func (a *API) lookupProfile(ctx context.Context, source, input string) (*linkedProfile, error) {
	input = strings.TrimSpace(input)
	id, idErr := uuid.Parse(input)
	if idErr != nil && !mcUsernameRe.MatchString(input) {
		return nil, newError(http.StatusBadRequest, "bad_request", "provide a Minecraft role name or UUID")
	}
	var src AuthSource
	found := false
	for _, candidate := range a.AuthSources {
		if candidate.Tag == source {
			src, found = candidate, true
			break
		}
	}
	if !found {
		return nil, newError(http.StatusBadRequest, "auth_source_unknown", "select a configured authentication source")
	}
	var target, method string
	var body io.Reader
	if src.Identity {
		method = http.MethodGet
		if idErr == nil {
			target = strings.TrimSuffix(src.URL, "/hasJoined") + "/profile/" + strings.ReplaceAll(id.String(), "-", "")
		} else {
			target = mojangProfileAPI + url.PathEscape(input)
		}
	} else {
		base := profileAPIBase(src)
		if base == "" {
			return nil, newError(http.StatusBadRequest, "auth_source_lookup_unsupported", "this source needs api_url for role lookup; game-code linking is still available")
		}
		if idErr == nil {
			method, target = http.MethodGet, base+"/sessionserver/session/minecraft/profile/"+strings.ReplaceAll(id.String(), "-", "")
		} else {
			method, target = http.MethodPost, base+"/api/profiles/minecraft"
			encoded, _ := json.Marshal([]string{input})
			body = bytes.NewReader(encoded)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Use the same bounded, redirect-free client as game authentication.
	resp, err := authHTTPClient.Do(req)
	if err != nil {
		return nil, newError(http.StatusServiceUnavailable, "auth_source_unavailable", "the selected authentication source is unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil, newError(http.StatusNotFound, "minecraft_profile_not_found", "no role matched in the selected source")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newError(http.StatusServiceUnavailable, "auth_source_unavailable", "the selected authentication source returned HTTP %d", resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<16))
	var profile sessionProfile
	if method == http.MethodPost {
		var profiles []sessionProfile
		if err := decoder.Decode(&profiles); err != nil {
			return nil, newError(http.StatusBadGateway, "auth_source_unavailable", "invalid profile response")
		}
		for _, candidate := range profiles {
			if strings.EqualFold(candidate.Name, input) {
				profile = candidate
				break
			}
		}
		if profile.ID == "" {
			return nil, newError(http.StatusNotFound, "minecraft_profile_not_found", "no role matched in the selected source")
		}
	} else if err := decoder.Decode(&profile); err != nil {
		return nil, newError(http.StatusBadGateway, "auth_source_unavailable", "invalid profile response")
	}
	profileID, err := uuid.Parse(profile.ID)
	if err != nil || !mcUsernameRe.MatchString(profile.Name) ||
		(idErr == nil && profileID != id) || (idErr != nil && !strings.EqualFold(profile.Name, input)) {
		return nil, newError(http.StatusBadGateway, "auth_source_unavailable", "the source returned a mismatched or invalid role")
	}
	canonical, err := canonicalProfileUUID(src, profile.ID)
	if err != nil {
		return nil, err
	}
	authSource := authSourceThirdParty
	if src.Identity {
		authSource = authSourceMojang
	}
	return &linkedProfile{Source: src.Tag, Name: profile.Name, ProfileUUID: profile.ID, MCUUID: canonical.String(), AuthSource: authSource}, nil
}
