package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"felis.lolicon.best/internal/config"
)

const authSourcesKey = "auth_sources"

type authSourceEntry struct {
	config.AuthSourceConfig
	Enabled bool `json:"enabled"`
}

type authSourcesView struct {
	Sources  []authSourceEntry `json:"sources"`
	Revision string            `json:"revision"`
	Managed  bool              `json:"managed"`
}

// AuthSourceSettings reuses platform_settings for the full control plane. Nano
// keeps its TOML-only sources. A durable override is read for each login, so all
// API replicas see the same list and a DB failure never revives a disabled root.
type AuthSourceSettings struct {
	Repo     Repo
	Defaults []AuthSource
}

func validateAuthSourceEntries(entries []authSourceEntry) error {
	if entries == nil || len(entries) > 32 {
		return fmt.Errorf("provide a sources array with at most 32 entries")
	}
	sources := make([]config.AuthSourceConfig, len(entries))
	for i, entry := range entries {
		sources[i] = entry.AuthSourceConfig
	}
	return config.ValidateAuthSources(sources)
}

func (s *AuthSourceSettings) read(ctx context.Context) (authSourcesView, []byte, error) {
	view := authSourcesView{Sources: []authSourceEntry{}}
	raw, err := s.Repo.GetSetting(ctx, authSourcesKey)
	switch {
	case errors.Is(err, ErrNotFound):
		for _, source := range s.Defaults {
			if !source.Identity {
				view.Sources = append(view.Sources, authSourceEntry{AuthSourceConfig: config.AuthSourceConfig{
					Tag: source.Tag, Prefix: source.Prefix, URL: source.URL, APIURL: source.APIURL,
				}, Enabled: true})
			}
		}
		raw = nil
	case err != nil:
		return view, nil, err
	default:
		if err := json.Unmarshal(raw, &view.Sources); err != nil {
			return view, nil, fmt.Errorf("auth sources: invalid stored configuration: %w", err)
		}
		view.Managed = true
	}
	if err := validateAuthSourceEntries(view.Sources); err != nil {
		return view, nil, err
	}
	canonical, _ := json.Marshal(view.Sources)
	sum := sha256.Sum256(canonical)
	view.Revision = hex.EncodeToString(sum[:])
	return view, raw, nil
}

func (a *API) currentAuthSources(ctx context.Context) ([]AuthSource, error) {
	if a.AuthSourceSettings == nil {
		return a.AuthSources, nil
	}
	view, _, err := a.AuthSourceSettings.read(ctx)
	if err != nil {
		return nil, err
	}
	sources := make([]AuthSource, 0, len(view.Sources)+1)
	for _, source := range a.AuthSourceSettings.Defaults {
		if source.Identity {
			sources = append(sources, source)
		}
	}
	for _, entry := range view.Sources {
		if entry.Enabled {
			sources = append(sources, AuthSource{Tag: entry.Tag, Prefix: entry.Prefix, URL: entry.URL, APIURL: entry.APIURL})
		}
	}
	return sources, nil
}

func (a *API) handleGetAuthSources(w http.ResponseWriter, r *http.Request) {
	if a.AuthSourceSettings == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "auth_sources_unavailable", "authentication source settings are not configured"))
		return
	}
	view, _, err := a.AuthSourceSettings.read(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleSetAuthSources(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if a.AuthSourceSettings == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "auth_sources_unavailable", "authentication source settings are not configured"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		Sources  []authSourceEntry `json:"sources"`
		Revision string            `json:"revision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if err := validateAuthSourceEntries(body.Sources); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "%s", err))
		return
	}
	view, expected, err := a.AuthSourceSettings.read(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.Revision != view.Revision {
		writeError(w, r, newError(http.StatusConflict, "auth_sources_changed", "authentication sources changed; reload before saving"))
		return
	}
	tags := make(map[string]bool, len(body.Sources))
	for _, source := range body.Sources {
		tags[source.Tag] = true
	}
	for _, existing := range view.Sources {
		if !tags[existing.Tag] {
			writeError(w, r, newError(http.StatusConflict, "auth_source_tag_locked", "saved source tags are permanent; disable the source instead of removing or renaming it"))
			return
		}
	}
	value, _ := json.Marshal(body.Sources)
	err = a.AuthSourceSettings.Repo.CompareAndSetSetting(r.Context(), authSourcesKey, expected, value)
	if errors.Is(err, ErrConflict) {
		writeError(w, r, newError(http.StatusConflict, "auth_sources_changed", "authentication sources changed; reload before saving"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	view.Sources, view.Managed = body.Sources, true
	sum := sha256.Sum256(value)
	view.Revision = hex.EncodeToString(sum[:])
	a.audit(r, "auth_sources.updated", "")
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleTestAuthSource(w http.ResponseWriter, r *http.Request) {
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var source authSourceEntry
	if err := decodeJSON(w, r, &source); err != nil {
		writeError(w, r, err)
		return
	}
	if err := validateAuthSourceEntries([]authSourceEntry{source}); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "%s", err))
		return
	}
	probeID, err := newPasskeyID()
	if err != nil {
		writeError(w, r, err)
		return
	}
	// A fresh random serverId has never joined: a healthy hasJoined endpoint
	// answers 204. Reuse authentication's timeout, TLS and redirect policy.
	started := time.Now()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		source.URL+"?username=FelisProbe&serverId="+url.QueryEscape(probeID), nil)
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp, err := authHTTPClient.Do(req)
	if err != nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "auth_source_unavailable", "the authentication endpoint could not be reached"))
		return
	}
	resp.Body.Close()
	writeJSON(w, http.StatusOK, map[string]any{"ok": resp.StatusCode == http.StatusNoContent, "status": resp.StatusCode, "elapsed_ms": time.Since(started).Milliseconds()})
}
