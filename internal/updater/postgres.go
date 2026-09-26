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

// postgresFeedURL is the PostgreSQL project's machine-readable release table, the one
// its own versioning page renders from. Checked 2026-09-25: an array with one entry per
// major, e.g. {"major":"13","latestMinor":"23","supported":false,"eolDate":"2025-11-13"}.
const postgresFeedURL = "https://www.postgresql.org/versions.json"

// postgresFeed discovers the newest minor release of the running PostgreSQL major.
//
// Felis runs PostgreSQL from an image the release pins, and moving to a new major is a
// dump and restore the operator plans. The report therefore compares within the major
// (a minor release is a new pin), and a major that is past its end of life is surfaced
// separately as a note.
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

// postgresRelease is one lookup's answer: the newest minor of the current major, and
// a note when that major is no longer supported.
type postgresRelease struct {
	latest updates.Version
	note   string
}

// majorKey is the feed's name for the major a version belongs to: "13" from 10 on,
// "9.6" before that, when the second number was still part of the major.
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
			rel.note += fmt.Sprintf("; the current major is %s (pg_upgrade, see docs/operations.md §4)", newest)
		}
	}
	return rel, nil
}
