package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"felis.lolicon.best/internal/naming"
)

const entryPolicyKey = "player_entry_policy"

type entryPolicy struct {
	Mode               string `json:"mode"`
	DefaultServer      string `json:"defaultServer"`
	RequireAccountLink bool   `json:"requireAccountLink"`
	OfflineAction      string `json:"offlineAction"`
	WaitingSpace       string `json:"waitingSpace"`
	FallbackServer     string `json:"fallbackServer"`
}

type entryPolicyView struct {
	entryPolicy
	Revision string `json:"revision"`
}

func defaultEntryPolicy() entryPolicy {
	// Preserve existing host routing and web association until the Owner saves a policy.
	return entryPolicy{Mode: "domain", RequireAccountLink: true, OfflineAction: "wake", WaitingSpace: "lobby"}
}

func (p entryPolicy) valid() bool {
	if p.Mode != "lobby" && p.Mode != "direct" && p.Mode != "domain" {
		return false
	}
	if p.OfflineAction != "wake" && p.OfflineAction != "fallback" && p.OfflineAction != "disconnect" {
		return false
	}
	if p.WaitingSpace != "login" && p.WaitingSpace != "lobby" {
		return false
	}
	if p.RequireAccountLink && p.WaitingSpace != "lobby" {
		return false
	}
	if p.Mode == "direct" && p.DefaultServer == "" {
		return false
	}
	if p.OfflineAction == "fallback" && (p.FallbackServer == "" || p.FallbackServer == p.DefaultServer) {
		return false
	}
	for _, name := range []string{p.DefaultServer, p.FallbackServer} {
		if name != "" && (naming.ValidateServerName(name) != nil || naming.IsSystemServer(name)) {
			return false
		}
	}
	return true
}

func (a *API) readEntryPolicy(ctx context.Context) (entryPolicyView, []byte, error) {
	view := entryPolicyView{entryPolicy: defaultEntryPolicy()}
	raw, err := a.Repo.GetSetting(ctx, entryPolicyKey)
	if errors.Is(err, ErrNotFound) {
		raw = nil
	} else if err != nil {
		return view, nil, err
	} else if err = json.Unmarshal(raw, &view.entryPolicy); err != nil {
		return view, nil, err
	}
	if !view.entryPolicy.valid() {
		return view, nil, errors.New("invalid player entry policy")
	}
	canonical, _ := json.Marshal(view.entryPolicy)
	sum := sha256.Sum256(canonical)
	view.Revision = hex.EncodeToString(sum[:])
	return view, raw, nil
}

func (a *API) handleGetEntryPolicy(w http.ResponseWriter, r *http.Request) {
	view, _, err := a.readEntryPolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, 200, view)
}

func (a *API) handleSetEntryPolicy(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body entryPolicyView
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !body.entryPolicy.valid() {
		writeError(w, r, newError(400, "bad_request", "invalid entry mode, target or offline policy"))
		return
	}
	for _, name := range []string{body.DefaultServer, body.FallbackServer} {
		if name == "" {
			continue
		}
		if _, err := a.Cluster.GetServer(r.Context(), name); err != nil {
			a.writeLookupError(w, r, err)
			return
		}
	}
	current, expected, err := a.readEntryPolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.Revision != current.Revision {
		writeError(w, r, newError(409, "conflict", "player entry policy changed; reload before saving"))
		return
	}
	raw, _ := json.Marshal(body.entryPolicy)
	if err = a.Repo.CompareAndSetSetting(r.Context(), entryPolicyKey, expected, raw); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "platform.entry_policy", "platform")
	view, _, err := a.readEntryPolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
