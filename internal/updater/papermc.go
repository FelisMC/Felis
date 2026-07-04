package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"felis.lolicon.best/internal/updates"
)

// defaultUserAgent identifies Felis to the PaperMC Fill v3 API. PaperMC's API usage
// policy asks consumers to send a descriptive User-Agent that names the application
// and carries contact info, and warns that generic/library-default agents (curl, wget,
// Go-http-client) may be rate-limited or blocked. Enforcement was NOT active on the
// project-metadata endpoint as of 2026-07-04 — a bare UA still returned HTTP 200 — so
// sending this is documented etiquette and future-proofing, not an empirically
// confirmed hard gate. It uses the public Felis module path as the contact and
// contains no operator-specific serving domain; a deployment can override it
// (paperMC.userAgent) with a SysAdmin contact from config.
const defaultUserAgent = "felis-updater/0.1 (+https://felis.lolicon.best)"

// paperMC discovers the latest STABLE version of a PaperMC project (Velocity, for
// Felis) from the Fill v3 API. The old v2 API (api.papermc.io) stopped serving
// builds on 2025-12-31 and was disabled 2026-07-01 — it now returns HTTP 410 — so
// Felis targets v3 at fill.papermc.io. baseURL, userAgent and hc are fields so the
// discovery logic is exercised against an httptest server without touching the
// network (see papermc_test.go, whose fixture is captured from the real v3 shape).
type paperMC struct {
	baseURL   string // e.g. "https://fill.papermc.io"
	userAgent string // descriptive UA with a contact (PaperMC usage-policy etiquette)
	hc        *http.Client
}

// newPaperMC builds a source pointed at the live Fill v3 endpoint with sane defaults.
func newPaperMC() paperMC {
	return paperMC{
		baseURL:   "https://fill.papermc.io",
		userAgent: defaultUserAgent,
		hc:        &http.Client{Timeout: 15 * time.Second},
	}
}

// projectResponse is the slice of GET /v3/projects/{project} that Felis reads. The
// live v3 shape (2026-07-04) is:
//
//	{"project":{"id":"velocity","name":"Velocity"},
//	 "versions":{"<group>":["3.4.0","3.4.0-SNAPSHOT", ...], ...}}
//
// versions is an object keyed by version-group, each value a list of published
// version strings mixing stable ("3.4.0") and prerelease ("3.4.0-SNAPSHOT"). Felis
// ignores both the grouping and the array order: it flattens every version, keeps
// only stable ones, and takes the max — so a regrouped or reordered feed yields the
// same answer.
type projectResponse struct {
	Versions map[string][]string `json:"versions"`
}

// latestStable returns the newest STABLE (non-prerelease) version published for
// project. It matters that this filters prereleases: for Velocity the newest overall
// version is routinely a "-SNAPSHOT" (e.g. 3.5.0-SNAPSHOT while the newest release is
// 3.4.0), and PlanUpdates only ever acts on a stable upgrade — a source that returned
// the SNAPSHOT would make the plan silently do nothing.
//
// It fails closed: a transport error, a non-200 status, an undecodable body, or a
// feed with no parseable stable version all return an error, so a garbled or
// SNAPSHOT-only feed never yields a bogus "latest" that could drive a spurious
// notify/apply. An individual unparseable tag is skipped, not fatal — one weird entry
// does not blind discovery to the rest.
func (p paperMC) latestStable(ctx context.Context, project string) (updates.Version, error) {
	url := fmt.Sprintf("%s/v3/projects/%s", p.baseURL, project)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return updates.Version{}, fmt.Errorf("papermc: build request for %s: %w", project, err)
	}
	ua := p.userAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")

	resp, err := p.hc.Do(req)
	if err != nil {
		return updates.Version{}, fmt.Errorf("papermc: get %s: %w", project, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return updates.Version{}, fmt.Errorf("papermc: %s returned HTTP %d", project, resp.StatusCode)
	}

	var pr projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return updates.Version{}, fmt.Errorf("papermc: decode %s: %w", project, err)
	}

	var best updates.Version
	found := false
	for _, group := range pr.Versions {
		for _, s := range group {
			v, err := updates.Parse(s)
			if err != nil || v.IsPrerelease() {
				continue // skip unparseable tags and prereleases (SNAPSHOT / rc)
			}
			if !found || v.After(best) {
				best, found = v, true
			}
		}
	}
	if !found {
		return updates.Version{}, fmt.Errorf("papermc: no stable release found for %s", project)
	}
	return best, nil
}
