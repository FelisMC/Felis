package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"felis.lolicon.best/internal/heartbeatsettings"
)

// HeartbeatView is GET /settings/heartbeat: whether the Owner set a heartbeat URL in the
// panel, the URL as it may be shown (scheme and host: the path is the check's key, which
// anyone holding it can ping in this host's name), and the revision a change must name.
// With none set here, the watchdog pings the host file's URL, if the installer wrote one.
type HeartbeatView struct {
	Set      bool   `json:"set"`
	Display  string `json:"display"`
	Revision string `json:"revision"`
}

func (a *API) heartbeatView(ctx context.Context) (HeartbeatView, []byte, error) {
	raw, err := a.Repo.GetSetting(ctx, heartbeatsettings.Key)
	if errors.Is(err, ErrNotFound) {
		raw, err = nil, nil
	}
	if err != nil {
		return HeartbeatView{}, nil, err
	}
	s, err := heartbeatsettings.Parse(raw)
	if err != nil {
		return HeartbeatView{}, nil, err
	}
	var v HeartbeatView
	if s != nil {
		v.Set, v.Display = true, heartbeatsettings.Redact(s.URL)
	}
	// The revision hashes the URL itself, so a change made elsewhere is caught even when the
	// shown part stays the same.
	canonical, _ := json.Marshal(s)
	sum := sha256.Sum256(canonical)
	v.Revision = hex.EncodeToString(sum[:])
	return v, raw, nil
}

func (a *API) handleGetHeartbeat(w http.ResponseWriter, r *http.Request) {
	v, _, err := a.heartbeatView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) handleSetHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		URL      string `json:"url"`
		Revision string `json:"revision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	u := strings.TrimSpace(body.URL)
	if err := heartbeatsettings.Check(u); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_heartbeat_url", "%v", err))
		return
	}
	a.writeHeartbeat(w, r, body.Revision, &heartbeatsettings.Settings{URL: u})
}

func (a *API) handleClearHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	v, _, err := a.heartbeatView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	a.writeHeartbeat(w, r, v.Revision, nil)
}

func (a *API) writeHeartbeat(w http.ResponseWriter, r *http.Request, revision string, s *heartbeatsettings.Settings) {
	current, expected, err := a.heartbeatView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if revision != current.Revision {
		writeError(w, r, newError(http.StatusConflict, "conflict", "the heartbeat setting changed; reload before saving"))
		return
	}
	raw := []byte("null")
	if s != nil {
		raw, _ = json.Marshal(s)
	}
	if err := a.Repo.CompareAndSetSetting(r.Context(), heartbeatsettings.Key, expected, raw); err != nil {
		writeError(w, r, err)
		return
	}
	action := "platform.heartbeat"
	if s == nil {
		action = "platform.heartbeat.clear"
	}
	a.audit(r, action, "platform")
	v, _, err := a.heartbeatView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
