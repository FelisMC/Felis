package api

import (
	"bytes"
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

// retiredAuthSource is a deleted source's tag, kept with the endpoint it pointed at.
// Player UUIDs derive from the tag, so the tag comes back only for that endpoint: under
// another Yggdrasil the same tag would hand those players' links, bans and data to
// whoever holds their native IDs there.
type retiredAuthSource struct {
	Tag string `json:"tag"`
	URL string `json:"url"`
}

type authSourcesView struct {
	Sources  []authSourceEntry   `json:"sources"`
	Retired  []retiredAuthSource `json:"retired"`
	Revision string              `json:"revision"`
	Managed  bool                `json:"managed"`
}

// storedAuthSources is the platform_settings value. Earlier releases stored the bare
// sources array, which still reads.
type storedAuthSources struct {
	Sources []authSourceEntry   `json:"sources"`
	Retired []retiredAuthSource `json:"retired"`
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
	view := authSourcesView{Sources: []authSourceEntry{}, Retired: []retiredAuthSource{}}
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
		var stored storedAuthSources
		var err error
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
			err = json.Unmarshal(raw, &stored.Sources)
		} else {
			err = json.Unmarshal(raw, &stored)
		}
		if err != nil {
			return view, nil, fmt.Errorf("auth sources: invalid stored configuration: %w", err)
		}
		view.Sources, view.Managed = stored.Sources, true
		if stored.Retired != nil {
			view.Retired = stored.Retired
		}
	}
	if err := validateAuthSourceEntries(view.Sources); err != nil {
		return view, nil, err
	}
	view.Revision = authSourcesRevision(view.Sources)
	return view, raw, nil
}

// authSourcesRevision is the opaque revision of a source list, the same however the
// list was stored.
func authSourcesRevision(sources []authSourceEntry) string {
	canonical, _ := json.Marshal(sources)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// retireAuthSources returns the retired tags after saving next over view: a saved tag
// missing from next is retired with its endpoint, and a retired tag that comes back with
// that same endpoint is restored. A retired tag that comes back with another endpoint is
// refused.
func retireAuthSources(view authSourcesView, next []authSourceEntry) ([]retiredAuthSource, error) {
	byTag := make(map[string]authSourceEntry, len(next))
	for _, source := range next {
		byTag[source.Tag] = source
	}
	retired := make([]retiredAuthSource, 0, len(view.Retired)+len(view.Sources))
	for _, old := range view.Retired {
		source, back := byTag[old.Tag]
		if !back {
			retired = append(retired, old)
			continue
		}
		if source.URL != old.URL {
			return nil, newError(http.StatusConflict, "auth_source_tag_retired",
				"the ID %q belonged to a deleted source at %s; reuse it only with that address, or choose another ID", old.Tag, old.URL)
		}
	}
	for _, existing := range view.Sources {
		if _, kept := byTag[existing.Tag]; !kept {
			retired = append(retired, retiredAuthSource{Tag: existing.Tag, URL: existing.URL})
		}
	}
	return retired, nil
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
	retired, err := retireAuthSources(view, body.Sources)
	if err != nil {
		writeError(w, r, err)
		return
	}
	value, _ := json.Marshal(storedAuthSources{Sources: body.Sources, Retired: retired})
	err = a.AuthSourceSettings.Repo.CompareAndSetSetting(r.Context(), authSourcesKey, expected, value)
	if errors.Is(err, ErrConflict) {
		writeError(w, r, newError(http.StatusConflict, "auth_sources_changed", "authentication sources changed; reload before saving"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	view.Sources, view.Retired, view.Managed = body.Sources, retired, true
	view.Revision = authSourcesRevision(body.Sources)
	a.audit(r, "auth_sources.updated", "")
	writeJSON(w, http.StatusOK, view)
}

// handleTestAuthSource probes an unsaved source from inside the cluster, so it asks for the
// same fresh proof as saving one: a stolen session must not get a free network probe.
func (a *API) handleTestAuthSource(w http.ResponseWriter, r *http.Request) {
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
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
