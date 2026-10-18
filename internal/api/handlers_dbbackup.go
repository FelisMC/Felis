package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"felis.lolicon.best/internal/dbbackup"
)

type dbBackupView struct {
	Last          *dbbackup.Status `json:"last"`
	Stale         bool             `json:"stale"`
	MaxAgeSeconds int64            `json:"max_age_seconds"`
}

// handleGetDBBackup treats only missing keys as no backup; store errors remain failures.
func (a *API) handleGetDBBackup(w http.ResponseWriter, r *http.Request) {
	view := dbBackupView{Stale: true, MaxAgeSeconds: int64(dbbackup.StaleAfter.Seconds())}
	raw, err := a.Repo.GetSetting(r.Context(), dbbackup.StatusKey)
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusOK, view)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	var st dbbackup.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		writeError(w, r, err)
		return
	}
	// Measure freshness from the daily bundle so newer manual or pre-migration backups cannot hide a stopped daily timer.
	if st.Label == dbbackup.LabelDaily {
		st.DailyAt = st.At
	}
	view.Last = &st
	view.Stale = a.now().Sub(st.DailyAt) > dbbackup.StaleAfter
	writeJSON(w, http.StatusOK, view)
}
