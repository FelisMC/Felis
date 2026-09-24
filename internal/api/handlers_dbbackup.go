package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"felis.lolicon.best/internal/dbbackup"
)

// Control-plane database backup freshness (admin-tier, read-only). The backups
// themselves run on the host — felis-db-backup.timer calls `felis db backup`,
// which records its newest success in platform_settings[dbbackup.StatusKey] — so
// the API can report them without reaching the host's backup directory. The
// panel shows this next to the update window: both answer "is it safe to change
// something on this install right now".

// dbBackupView is the wire shape. Last is null until the first backup has been
// recorded; Stale is true for a missing record too, so the panel has a single
// flag for "nobody could restore today's state".
type dbBackupView struct {
	Last          *dbbackup.Status `json:"last"`
	Stale         bool             `json:"stale"`
	MaxAgeSeconds int64            `json:"max_age_seconds"`
}

// handleGetDBBackup reports the newest recorded control-plane database backup.
// Only a missing key reads as "never"; any other store error is a 500 so a DB
// blip is never shown as a healthy or an absent backup.
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
	view.Last = &st
	view.Stale = st.At.IsZero() || a.now().Sub(st.At) > dbbackup.StaleAfter
	writeJSON(w, http.StatusOK, view)
}
