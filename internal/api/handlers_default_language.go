package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

const defaultLanguageKey = "default_language"

// An empty language follows the browser.
type defaultLanguage struct {
	Language string `json:"language"`
}

type defaultLanguageView struct {
	defaultLanguage
	Revision string `json:"revision"`
}

type panelLanguageView struct {
	Language string `json:"language"`
}

func (l defaultLanguage) valid() bool {
	return l.Language == "" || l.Language == "zh-CN" || l.Language == "en-US"
}

func (a *API) readDefaultLanguage(ctx context.Context) (defaultLanguageView, []byte, error) {
	var view defaultLanguageView
	raw, err := a.Repo.GetSetting(ctx, defaultLanguageKey)
	switch {
	case errors.Is(err, ErrNotFound):
		raw = nil
	case err != nil:
		return view, nil, err
	default:
		if err := json.Unmarshal(raw, &view.defaultLanguage); err != nil {
			return view, nil, err
		}
		if !view.defaultLanguage.valid() {
			return view, nil, errors.New("invalid persisted default language")
		}
	}
	canonical, _ := json.Marshal(view.defaultLanguage)
	sum := sha256.Sum256(canonical)
	view.Revision = hex.EncodeToString(sum[:])
	return view, raw, nil
}

func (a *API) handleGetDefaultLanguage(w http.ResponseWriter, r *http.Request) {
	view, _, err := a.readDefaultLanguage(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleSetDefaultLanguage(w http.ResponseWriter, r *http.Request) {
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body defaultLanguageView
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !body.defaultLanguage.valid() {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "language must be empty, zh-CN or en-US"))
		return
	}
	current, expected, err := a.readDefaultLanguage(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.Revision != current.Revision {
		writeError(w, r, newError(http.StatusConflict, "conflict", "default language changed; reload before saving"))
		return
	}
	raw, _ := json.Marshal(body.defaultLanguage)
	if err := a.Repo.CompareAndSetSetting(r.Context(), defaultLanguageKey, expected, raw); err != nil {
		writeError(w, r, err)
		return
	}
	p := principalFromContext(r.Context())
	entry := AuditEntry{
		Actor:      auditActor(p),
		Action:     "platform.default_language",
		ServerName: "platform",
		Payload:    auditPayload(map[string]any{"from": current.Language, "to": body.Language}),
	}
	if p != nil {
		entry.ActorUserID = p.UserID
	}
	a.auditEntry(r, entry)
	view, _, err := a.readDefaultLanguage(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handlePanelLanguage answers 200 with an empty language on store failure so the panel falls back to the browser language.
func (a *API) handlePanelLanguage(w http.ResponseWriter, r *http.Request) {
	view, _, err := a.readDefaultLanguage(r.Context())
	if err != nil {
		log.Printf("panel language: %v", err)
		view = defaultLanguageView{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, panelLanguageView{Language: view.Language})
}
