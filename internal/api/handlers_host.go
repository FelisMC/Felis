package api

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *API) handleHostOverview(w http.ResponseWriter, r *http.Request) {
	if !a.nodeControlReady(w, r) {
		return
	}
	raw, err := a.NodeControl.Host(r.Context())
	if err != nil {
		a.writeNodeControlError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

func (a *API) handleHostQuiet(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !a.requireReauth(w, r, p) || !a.nodeControlReady(w, r) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		Until *time.Time `json:"until"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	var until time.Time
	if body.Until != nil {
		until = body.Until.UTC()
		if !until.After(time.Now()) || until.After(time.Now().Add(7*24*time.Hour)) {
			writeError(w, r, newError(400, "bad_request", "a hold ends within the next seven days"))
			return
		}
	}
	if err := a.NodeControl.Quiet(r.Context(), until); err != nil {
		a.writeNodeControlError(w, r, err)
		return
	}
	detail := "lifted"
	if !until.IsZero() {
		detail = until.Format(time.RFC3339)
	}
	a.audit(r, "platform.host.quiet", detail)
	w.WriteHeader(http.StatusNoContent)
}

// handleOffsiteKey returns the decryption key, which must be retained separately from this host for recovery.
func (a *API) handleOffsiteKey(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) || !a.nodeControlReady(w, r) {
		return
	}
	key, err := a.NodeControl.OffsiteKey(r.Context())
	if err != nil {
		a.writeNodeControlError(w, r, err)
		return
	}
	a.audit(r, "platform.host.offsite-key", "")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"key": key})
}

func (a *API) handleNodeTaskArtifact(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) || !a.nodeControlReady(w, r) {
		return
	}
	id := r.PathValue("id")
	body, size, name, err := a.NodeControl.Artifact(r.Context(), id)
	if err != nil {
		a.writeNodeControlError(w, r, err)
		return
	}
	defer body.Close()
	if name == "" || name == "." || strings.ContainsAny(name, "\"\\/\r\n") {
		name = "felis-" + id
	}
	contentType := "application/octet-stream"
	if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
		contentType = "application/gzip"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Cache-Control", "no-store")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	a.audit(r, "platform.host.download", id)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute))
	_, _ = io.Copy(w, body)
}

func (a *API) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if a.AuditExport == nil {
		writeError(w, r, newError(503, "audit_export_unavailable", "audit export is not available"))
		return
	}
	bound := func(key string) (time.Time, error) {
		v := r.URL.Query().Get(key)
		if v == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.DateOnly, v)
		if err != nil {
			return time.Time{}, newError(400, "bad_request", "%s must be a day such as 2026-01-31", key)
		}
		return t, nil
	}
	since, err := bound("since")
	if err != nil {
		writeError(w, r, err)
		return
	}
	until, err := bound("until")
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		writeError(w, r, newError(400, "bad_request", "until must come after since"))
		return
	}
	name := "felis-audit"
	if !since.IsZero() {
		name += "-from-" + since.Format(time.DateOnly)
	}
	if !until.IsZero() {
		name += "-before-" + until.Format(time.DateOnly)
	}
	a.audit(r, "platform.audit.export", strings.TrimPrefix(name, "felis-audit"))
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".jsonl"))
	w.Header().Set("Cache-Control", "no-store")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute))
	_, _ = a.AuditExport(r.Context(), since, until, w)
}
