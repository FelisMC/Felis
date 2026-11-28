package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
)

const (
	ModeTeam            = "team"
	ModeCommunity       = "community"
	platformFeaturesKey = "platform_features"
)

type platformFeatures struct {
	Mode             string `json:"mode"`
	CustomImages     bool   `json:"customImages"`
	AllowSubmissions bool   `json:"allowSubmissions"`
}

type platformFeaturesView struct {
	Mode               string `json:"mode"`
	LobbyAvailable     bool   `json:"lobbyAvailable"`
	UseLobby           bool   `json:"useLobby"`
	RequireAccountLink bool   `json:"requireAccountLink"`
	CustomImages       bool   `json:"customImages"`
	AllowSubmissions   bool   `json:"allowSubmissions"`
	Revision           string `json:"revision"`
}

type setFeaturesRequest struct {
	Mode                   string `json:"mode"`
	UseLobby               *bool  `json:"useLobby,omitempty"`
	CustomImages           bool   `json:"customImages"`
	AllowSubmissions       bool   `json:"allowSubmissions"`
	SyncRequireAccountLink *bool  `json:"syncRequireAccountLink,omitempty"`
	Revision               string `json:"revision"`
}

var (
	errFeatureDisabled = newError(http.StatusForbidden, "feature_disabled", "this feature is disabled in the current platform mode")
	errTeamModeOnly    = newError(http.StatusForbidden, "team_mode_staff_only", "the panel is restricted to staff and server collaborators in team mode")
)

func validPlatformMode(mode string) bool {
	return mode == ModeTeam || mode == ModeCommunity
}

func defaultPlatformFeatures(mode string, customImages, allowSubmissions bool) platformFeatures {
	if !validPlatformMode(mode) {
		mode = ModeTeam
	}
	return platformFeatures{
		Mode:             mode,
		CustomImages:     customImages,
		AllowSubmissions: allowSubmissions,
	}
}

func featuresRevision(f platformFeatures, useLobby, requireAccountLink bool) string {
	canonical, _ := json.Marshal(struct {
		Mode               string `json:"mode"`
		CustomImages       bool   `json:"customImages"`
		AllowSubmissions   bool   `json:"allowSubmissions"`
		UseLobby           bool   `json:"useLobby"`
		RequireAccountLink bool   `json:"requireAccountLink"`
	}{
		Mode:               f.Mode,
		CustomImages:       f.CustomImages,
		AllowSubmissions:   f.AllowSubmissions,
		UseLobby:           useLobby,
		RequireAccountLink: requireAccountLink,
	})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func applyEntryPolicySync(p entryPolicy, noLobby bool, useLobby, requireLink *bool) (entryPolicy, bool) {
	next := p
	if useLobby != nil {
		next.UseLobby = *useLobby
		if !next.UseLobby {
			next = next.withoutLobby()
		} else if next.RequireAccountLink {
			next.WaitingSpace = "lobby"
		}
	}
	if requireLink != nil {
		next.RequireAccountLink = *requireLink
		if next.UseLobby && next.RequireAccountLink {
			next.WaitingSpace = "lobby"
		}
	}
	if noLobby {
		next = next.withoutLobby()
	}
	return next, next != p
}

func (a *API) readStoredFeatures(ctx context.Context) (platformFeatures, []byte, error) {
	def := defaultPlatformFeatures(a.DefaultMode, a.CustomImagesByDefault, a.AllowSubmissionsDefault)
	raw, err := a.Repo.GetSetting(ctx, platformFeaturesKey)
	if errors.Is(err, ErrNotFound) {
		return def, nil, nil
	}
	if err != nil {
		return def, nil, err
	}
	f := def
	if err := json.Unmarshal(raw, &f); err != nil {
		return def, nil, err
	}
	if !validPlatformMode(f.Mode) {
		return def, nil, errors.New("invalid platform mode")
	}
	return f, raw, nil
}

func (a *API) readFeaturesView(ctx context.Context) (platformFeaturesView, platformFeatures, []byte, entryPolicyView, []byte, error) {
	f, rawFeatures, err := a.readStoredFeatures(ctx)
	if err != nil {
		return platformFeaturesView{}, f, nil, entryPolicyView{}, nil, err
	}
	ep, rawEntry, err := a.readEntryPolicyForMode(ctx, f.Mode == ModeTeam)
	if err != nil {
		return platformFeaturesView{}, f, nil, ep, nil, err
	}
	view := platformFeaturesView{
		Mode:               f.Mode,
		LobbyAvailable:     !a.NoLobby,
		UseLobby:           ep.UseLobby,
		RequireAccountLink: ep.RequireAccountLink,
		CustomImages:       f.CustomImages,
		AllowSubmissions:   f.AllowSubmissions,
		Revision:           featuresRevision(f, ep.UseLobby, ep.RequireAccountLink),
	}
	return view, f, rawFeatures, ep, rawEntry, nil
}

func (a *API) effectiveMode(ctx context.Context) (string, error) {
	f, _, err := a.readStoredFeatures(ctx)
	if err != nil {
		return "", err
	}
	return f.Mode, nil
}

func (a *API) panelPortalAllowed(ctx context.Context, userID, role string) (bool, error) {
	if a.Nano || staffRole(role) {
		return true, nil
	}
	mode, err := a.effectiveMode(ctx)
	if err != nil {
		return false, err
	}
	if mode != ModeTeam {
		return true, nil
	}
	return a.userHasCollaboratorAccess(ctx, userID)
}

func (a *API) userHasCollaboratorAccess(ctx context.Context, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	return a.Repo.HasAnyCollaboratorAccess(ctx, userID)
}

func (a *API) requireCommunityMode(w http.ResponseWriter, r *http.Request) bool {
	mode, err := a.effectiveMode(r.Context())
	if err != nil {
		writeError(w, r, err)
		return false
	}
	if mode != ModeCommunity {
		writeError(w, r, errFeatureDisabled)
		return false
	}
	return true
}

func (a *API) requireCustomImages(w http.ResponseWriter, r *http.Request) bool {
	f, _, err := a.readStoredFeatures(r.Context())
	if err != nil {
		writeError(w, r, err)
		return false
	}
	if !f.CustomImages {
		writeError(w, r, errFeatureDisabled)
		return false
	}
	return true
}

func (a *API) requireSubmissionsAllowed(w http.ResponseWriter, r *http.Request) bool {
	f, _, err := a.readStoredFeatures(r.Context())
	if err != nil {
		writeError(w, r, err)
		return false
	}
	if !f.CustomImages || !f.AllowSubmissions {
		writeError(w, r, errFeatureDisabled)
		return false
	}
	return true
}

func (a *API) handleGetFeatures(w http.ResponseWriter, r *http.Request) {
	view, _, _, _, _, err := a.readFeaturesView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleSetFeatures(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body setFeaturesRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !validPlatformMode(body.Mode) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "mode must be team or community"))
		return
	}
	if body.UseLobby != nil && *body.UseLobby && a.NoLobby {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "formal lobby is not installed on this host"))
		return
	}
	ctx := r.Context()
	currentView, _, expectedFeatures, currentEntry, expectedEntry, err := a.readFeaturesView(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.Revision != currentView.Revision {
		writeError(w, r, newError(http.StatusConflict, "conflict", "platform features changed; reload before saving"))
		return
	}

	nextEntry, entryChanged := applyEntryPolicySync(currentEntry.entryPolicy, a.NoLobby, body.UseLobby, body.SyncRequireAccountLink)
	if entryChanged && !nextEntry.valid(a.NoLobby) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "resulting entry policy is invalid"))
		return
	}
	if body.UseLobby != nil && !a.NoLobby && *body.UseLobby != currentEntry.UseLobby {
		if err := a.syncLobbyServer(ctx, *body.UseLobby); err != nil {
			writeError(w, r, err)
			return
		}
	}

	nextFeatures := platformFeatures{
		Mode:             body.Mode,
		CustomImages:     body.CustomImages,
		AllowSubmissions: body.AllowSubmissions,
	}
	rawFeatures, _ := json.Marshal(nextFeatures)
	if err := a.Repo.CompareAndSetSetting(ctx, platformFeaturesKey, expectedFeatures, rawFeatures); err != nil {
		writeError(w, r, err)
		return
	}
	if entryChanged {
		rawEntry, _ := json.Marshal(nextEntry)
		if err := a.Repo.CompareAndSetSetting(ctx, entryPolicyKey, expectedEntry, rawEntry); err != nil {
			writeError(w, r, err)
			return
		}
	}
	a.auditDetail(r, "platform.features", "platform", map[string]any{
		"mode":             nextFeatures.Mode,
		"useLobby":         nextEntry.UseLobby,
		"customImages":     nextFeatures.CustomImages,
		"allowSubmissions": nextFeatures.AllowSubmissions,
	})
	view, _, _, _, _, err := a.readFeaturesView(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
