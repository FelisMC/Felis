package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"felis.lolicon.best/internal/updates"
)

// Updates require explicit application within the global maintenance window; unreadable windows refuse even manual overrides.

const updateWindowKey = "update_window"

// updateWindow is an absolute [start,end) interval; null endpoints mean unset.
type updateWindow struct {
	Start *time.Time `json:"start"`
	End   *time.Time `json:"end"`
}

// handleGetUpdateWindow treats only missing keys as unset; store errors remain failures.
func (a *API) handleGetUpdateWindow(w http.ResponseWriter, r *http.Request) {
	raw, err := a.Repo.GetSetting(r.Context(), updateWindowKey)
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusOK, updateWindow{})
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	var win updateWindow
	if err := json.Unmarshal(raw, &win); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, win)
}

// handleSetUpdateWindow requires both valid endpoints or both null; past windows are allowed but never reopen.
func (a *API) handleSetUpdateWindow(w http.ResponseWriter, r *http.Request) {
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body updateWindow
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	switch {
	case body.Start == nil && body.End == nil:
	case body.Start == nil || body.End == nil:
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"start and end must both be set or both be null"))
		return
	case !body.End.After(*body.Start):
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"end must be after start"))
		return
	}

	value, err := json.Marshal(body)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.Repo.SetSetting(r.Context(), updateWindowKey, value); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "updates.window_set", "")
	writeJSON(w, http.StatusOK, body)
}

type updateReportView struct {
	Report        *updates.StatusReport `json:"report"`
	Stale         bool                  `json:"stale"`
	MaxAgeSeconds int64                 `json:"max_age_seconds"`
}

// handleGetUpdateReport treats only missing keys as never checked; store errors remain failures.
func (a *API) handleGetUpdateReport(w http.ResponseWriter, r *http.Request) {
	view := updateReportView{Stale: true, MaxAgeSeconds: int64(updates.StatusStaleAfter.Seconds())}
	raw, err := a.Repo.GetSetting(r.Context(), updates.StatusKey)
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusOK, view)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	var rep updates.StatusReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		writeError(w, r, err)
		return
	}
	if rep.Components == nil {
		rep.Components = []updates.ComponentStatus{}
	}
	view.Report = &rep
	view.Stale = a.now().Sub(rep.CheckedAt) > updates.StatusStaleAfter
	writeJSON(w, http.StatusOK, view)
}
