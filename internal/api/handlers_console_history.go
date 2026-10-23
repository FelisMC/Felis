package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
)

func (a *API) handleConsoleHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name, ok := a.authorizeServerFiles(w, r)
	if !ok {
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > fileedit.MaxLogTailCursorBytes {
		writeError(w, r, newError(http.StatusBadRequest, "bad_path", "the log cursor exceeds 256 bytes"))
		return
	}
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if info.DesiredState != string(v1alpha1.DesiredStopped) || info.Ready ||
		info.Phase == string(v1alpha1.PhaseRunning) || info.Phase == string(v1alpha1.PhaseStarting) || info.Phase == string(v1alpha1.PhaseStopping) {
		writeError(w, r, newError(http.StatusConflict, "server_transitioning", "wait for the server to stop completely before reading its stored logs"))
		return
	}
	if err := a.Cluster.CheckFileAccess(r.Context(), name); err != nil {
		a.writeLookupError(w, r, maintenanceError(err, ""))
		return
	}
	exists, err := a.Cluster.WorldVolumeExists(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !exists {
		if cursor != "" {
			writeError(w, r, newError(http.StatusConflict, "file_changed", "the stored log volume no longer exists; reload the log"))
			return
		}
		sum := sha256.Sum256(nil)
		writeJSON(w, http.StatusOK, fileedit.LogTailPage{Content: []byte{}, ContentSHA256: hex.EncodeToString(sum[:])})
		return
	}
	if a.Files == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "files_unavailable", "the file editor is not configured"))
		return
	}
	page, err := a.Files.LogTail(r.Context(), name, cursor)
	if err != nil {
		writeFileEditError(w, r, err)
		return
	}
	if page.Content == nil {
		page.Content = []byte{}
	}
	writeJSON(w, http.StatusOK, page)
}
