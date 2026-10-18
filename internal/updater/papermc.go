package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"felis.lolicon.best/internal/updates"
)

// defaultUserAgent identifies the application and contact as requested by PaperMC's API usage policy.
const defaultUserAgent = "felis-updater/0.1 (+https://felis.lolicon.best)"

type paperMC struct {
	baseURL   string
	userAgent string
	hc        *http.Client
}

func newPaperMC() paperMC {
	return paperMC{
		baseURL:   "https://fill.papermc.io",
		userAgent: defaultUserAgent,
		hc:        &http.Client{Timeout: 15 * time.Second},
	}
}

// projectResponse groups mixed stable and prerelease version strings; discovery ignores grouping and feed order.
type projectResponse struct {
	Versions map[string][]string `json:"versions"`
}

// latestStable selects the maximum parseable stable version and fails if none exists, skipping malformed individual tags.
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
				continue
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
