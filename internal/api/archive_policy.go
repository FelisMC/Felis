package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"felis.lolicon.best/internal/archivepolicy"
	"felis.lolicon.best/internal/reaper"
)

const archivePolicyTTL = 15 * time.Second

// ArchivePolicy prefers panel retention over host config.
type ArchivePolicy struct {
	Repo      Repo
	Host      reaper.Config
	HostAudit time.Duration

	mu     sync.Mutex
	at     time.Time
	cached *archivepolicy.Policy
	read   bool
}

// panel is nil when unset; unreadable rows are logged and fall back to host retention.
func (p *ArchivePolicy) panel(ctx context.Context) *archivepolicy.Policy {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.read && time.Since(p.at) < archivePolicyTTL {
		return p.cached
	}
	raw, err := p.Repo.GetSetting(ctx, archivepolicy.Key)
	switch {
	case errors.Is(err, ErrNotFound):
		raw = nil
	case err != nil:
		log.Printf("api: read the retention policy: %v; felis.toml stands", err)
		return p.cached
	}
	parsed, err := archivepolicy.Parse(raw)
	if err != nil {
		log.Printf("api: %v; felis.toml stands", err)
	}
	p.cached, p.at, p.read = parsed, time.Now(), true
	return parsed
}

func (p *ArchivePolicy) invalidate() {
	p.mu.Lock()
	p.read = false
	p.mu.Unlock()
}

func (p *ArchivePolicy) Reaper(ctx context.Context) reaper.Config {
	rc := p.Host
	if pol := p.panel(ctx); pol != nil {
		pol.Apply(&rc)
	}
	return rc
}

// Audit=0 retains all rows.
func (p *ArchivePolicy) Audit(ctx context.Context) time.Duration {
	if pol := p.panel(ctx); pol != nil {
		return pol.Audit()
	}
	return p.HostAudit
}

func (a *API) backupCooldown(ctx context.Context) time.Duration {
	if a.ArchivePolicy != nil {
		return a.ArchivePolicy.Reaper(ctx).ManualCooldown
	}
	return a.BackupCooldown
}

func (a *API) backupStoreCap(ctx context.Context) int64 {
	if a.ArchivePolicy != nil {
		return a.ArchivePolicy.Reaper(ctx).MaxLocalBytes
	}
	return a.BackupStoreCap
}

type ArchivePolicyView struct {
	archivepolicy.Policy
	Source   string `json:"source"`
	Revision string `json:"revision"`
}

func (a *API) archivePolicyView(ctx context.Context) (ArchivePolicyView, []byte, error) {
	raw, err := a.Repo.GetSetting(ctx, archivepolicy.Key)
	if errors.Is(err, ErrNotFound) {
		raw, err = nil, nil
	}
	if err != nil {
		return ArchivePolicyView{}, nil, err
	}
	saved, err := archivepolicy.Parse(raw)
	if err != nil {
		return ArchivePolicyView{}, nil, err
	}
	view := ArchivePolicyView{Source: "host", Policy: archivepolicy.FromConfig(a.ArchivePolicy.Host, a.ArchivePolicy.HostAudit)}
	if saved != nil {
		view.Policy, view.Source = *saved, "panel"
	}
	// Hash canonical JSON because Postgres jsonb does not preserve input bytes.
	canonical, _ := json.Marshal(struct {
		archivepolicy.Policy
		Source string
	}{view.Policy, view.Source})
	sum := sha256.Sum256(canonical)
	view.Revision = hex.EncodeToString(sum[:])
	return view, raw, nil
}

func (a *API) archivePolicyReady(w http.ResponseWriter, r *http.Request) bool {
	if a.ArchivePolicy == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "archive_policy_unavailable", "retention settings are not available on this install"))
		return false
	}
	return true
}

func (a *API) handleGetArchivePolicy(w http.ResponseWriter, r *http.Request) {
	if !a.archivePolicyReady(w, r) {
		return
	}
	view, _, err := a.archivePolicyView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleSetArchivePolicy(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) || !a.archivePolicyReady(w, r) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		archivepolicy.Policy
		Revision string `json:"revision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if err := body.Validate(); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "%v", err))
		return
	}
	a.writeArchivePolicy(w, r, body.Revision, &body.Policy)
}

func (a *API) handleClearArchivePolicy(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) || !a.archivePolicyReady(w, r) {
		return
	}
	current, _, err := a.archivePolicyView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	a.writeArchivePolicy(w, r, current.Revision, nil)
}

// writeArchivePolicy uses compare-and-set; nil stores JSON null.
func (a *API) writeArchivePolicy(w http.ResponseWriter, r *http.Request, revision string, p *archivepolicy.Policy) {
	current, expected, err := a.archivePolicyView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if revision != current.Revision {
		writeError(w, r, newError(http.StatusConflict, "conflict", "the retention settings changed; reload before saving"))
		return
	}
	raw := []byte("null")
	if p != nil {
		raw, _ = json.Marshal(p)
	}
	if err := a.Repo.CompareAndSetSetting(r.Context(), archivepolicy.Key, expected, raw); err != nil {
		writeError(w, r, err)
		return
	}
	a.ArchivePolicy.invalidate()
	action := "platform.archive_policy"
	if p == nil {
		action = "platform.archive_policy.clear"
	}
	a.audit(r, action, "platform")
	view, _, err := a.archivePolicyView(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
