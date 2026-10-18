package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"felis.lolicon.best/internal/updates"
)

const postgresFeedURL = "https://www.postgresql.org/versions.json"

// postgresFeed compares minor releases within the running major; major upgrades require a planned dump and restore.
type postgresFeed struct {
	url       string
	userAgent string
	hc        *http.Client
}

func newPostgresFeed() postgresFeed {
	return postgresFeed{url: postgresFeedURL, userAgent: defaultUserAgent, hc: &http.Client{Timeout: 15 * time.Second}}
}

type postgresMajor struct {
	Major       string `json:"major"`
	LatestMinor string `json:"latestMinor"`
	Supported   bool   `json:"supported"`
	Current     bool   `json:"current"`
	EOLDate     string `json:"eolDate"`
}

type postgresRelease struct {
	latest updates.Version
	note   string
}

// majorKey uses one version component from PostgreSQL 10 onward and two for older releases.
func majorKey(v updates.Version) string {
	if v.Major < 10 {
		return fmt.Sprintf("%d.%d", v.Major, v.Minor)
	}
	return strconv.Itoa(v.Major)
}

func (p postgresFeed) latest(ctx context.Context, current updates.Version) (postgresRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return postgresRelease{}, fmt.Errorf("postgresql: build request: %w", err)
	}
	req.Header.Set("User-Agent", p.userAgent)
	resp, err := p.hc.Do(req)
	if err != nil {
		return postgresRelease{}, fmt.Errorf("postgresql: get %s: %w", p.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return postgresRelease{}, fmt.Errorf("postgresql: %s returned HTTP %d", p.url, resp.StatusCode)
	}
	var majors []postgresMajor
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&majors); err != nil {
		return postgresRelease{}, fmt.Errorf("postgresql: decode %s: %w", p.url, err)
	}

	key := majorKey(current)
	var mine *postgresMajor
	newest := ""
	for i := range majors {
		if majors[i].Major == key {
			mine = &majors[i]
		}
		if majors[i].Current {
			newest = majors[i].Major
		}
	}
	if mine == nil {
		return postgresRelease{}, fmt.Errorf("postgresql: the release feed has no major %s", key)
	}
	latest, err := updates.Parse(mine.Major + "." + mine.LatestMinor)
	if err != nil {
		return postgresRelease{}, fmt.Errorf("postgresql: major %s latest minor %q: %w", key, mine.LatestMinor, err)
	}
	rel := postgresRelease{latest: latest}
	if !mine.Supported {
		rel.note = fmt.Sprintf("PostgreSQL %s reached end of life on %s and gets no more fixes", key, mine.EOLDate)
		if newest != "" {
			rel.note += fmt.Sprintf("; the current major is %s (a dump and restore, see https://docs.felismc.com/en/operations/upgrade#postgresql-major)", newest)
		}
	}
	return rel, nil
}
