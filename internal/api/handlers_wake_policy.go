package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

const wakePolicyKey = "wake_policy"

type wakePolicy struct {
	MaxRunningServers   int `json:"maxRunningServers"`
	WakeCooldownSeconds int `json:"wakeCooldownSeconds"`
}
type wakePolicyView struct {
	wakePolicy
	Revision string `json:"revision"`
	Managed  bool   `json:"managed"`
}

func (a *API) readWakePolicy(ctx context.Context) (wakePolicyView, []byte, error) {
	view := wakePolicyView{wakePolicy: wakePolicy{a.MaxRunningServers, int(a.WakeCooldown / time.Second)}}
	raw, err := a.Repo.GetSetting(ctx, wakePolicyKey)
	switch {
	case errors.Is(err, ErrNotFound):
		raw = nil
	case err != nil:
		return view, nil, err
	default:
		if err := json.Unmarshal(raw, &view.wakePolicy); err != nil {
			return view, nil, err
		}
		if !view.wakePolicy.valid() {
			return view, nil, errors.New("invalid persisted wake policy")
		}
		view.Managed = true
	}
	canonical, _ := json.Marshal(view.wakePolicy)
	sum := sha256.Sum256(canonical)
	view.Revision = hex.EncodeToString(sum[:])
	return view, raw, nil
}

func (a *API) handleGetWakePolicy(w http.ResponseWriter, r *http.Request) {
	view, _, err := a.readWakePolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleSetWakePolicy(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		wakePolicy
		Revision string `json:"revision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !body.wakePolicy.valid() {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "running limit must be 0–10000 and wake cooldown 0–3600 seconds"))
		return
	}
	current, expected, err := a.readWakePolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.Revision != current.Revision {
		writeError(w, r, newError(http.StatusConflict, "conflict", "platform policy changed; reload before saving"))
		return
	}
	raw, _ := json.Marshal(body.wakePolicy)
	if err := a.Repo.CompareAndSetSetting(r.Context(), wakePolicyKey, expected, raw); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "platform.wake_policy", "platform")
	view, _, err := a.readWakePolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (p wakePolicyView) cooldown(fallback time.Duration) time.Duration {
	if !p.Managed {
		return fallback
	}
	return time.Duration(p.WakeCooldownSeconds) * time.Second
}

func (p wakePolicy) valid() bool {
	return p.MaxRunningServers >= 0 && p.MaxRunningServers <= 10000 && p.WakeCooldownSeconds >= 0 && p.WakeCooldownSeconds <= 3600
}
