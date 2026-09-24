package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postgresFeedFixture is a slice of https://www.postgresql.org/versions.json as served
// on 2026-09-25: a pre-10 major, an end-of-life major, and the current one.
const postgresFeedFixture = `[
{"current":false,"eolDate":"2021-11-11","firstRelDate":"2016-09-29","latestMinor":"24","major":"9.6","relDate":"2021-11-11","supported":false},
{"current":false,"eolDate":"2025-11-13","firstRelDate":"2020-09-24","latestMinor":"23","major":"13","relDate":"2025-11-13","supported":false},
{"current":false,"eolDate":"2029-11-08","firstRelDate":"2024-09-26","latestMinor":"10","major":"17","relDate":"2026-08-13","supported":true},
{"current":true,"eolDate":"2030-11-14","firstRelDate":"2025-09-25","latestMinor":"6","major":"18","relDate":"2026-08-13","supported":true}
]`

func pgFixtureServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func newTestPostgresFeed(srv *httptest.Server) postgresFeed {
	return postgresFeed{url: srv.URL, userAgent: "felis-updater/0.1", hc: srv.Client()}
}

func TestPostgresFeedComparesWithinTheMajor(t *testing.T) {
	srv := pgFixtureServer(postgresFeedFixture)
	defer srv.Close()
	cases := []struct {
		current, latest string
		note            []string
	}{
		{"17.4", "17.10", nil},
		{"18.6", "18.6", nil},
		{"13.22", "13.23", []string{"PostgreSQL 13 reached end of life on 2025-11-13", "current major is 18"}},
		{"9.6.3", "9.6.24", []string{"PostgreSQL 9.6 reached end of life"}},
	}
	for _, c := range cases {
		rel, err := newTestPostgresFeed(srv).latest(context.Background(), mustV(t, c.current))
		if err != nil {
			t.Fatalf("latest(%s): %v", c.current, err)
		}
		if rel.latest.Compare(mustV(t, c.latest)) != 0 {
			t.Errorf("latest(%s) = %s, want %s", c.current, rel.latest, c.latest)
		}
		if len(c.note) == 0 && rel.note != "" {
			t.Errorf("latest(%s) note = %q, want none for a supported major", c.current, rel.note)
		}
		for _, w := range c.note {
			if !strings.Contains(rel.note, w) {
				t.Errorf("latest(%s) note = %q, want it to mention %q", c.current, rel.note, w)
			}
		}
	}
}

func TestPostgresFeedFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"unknown major": postgresFeedFixture,
		"garbled feed":  `{"not":"a list"}`,
		"garbled minor": `[{"major":"16","latestMinor":"x","supported":true}]`,
	} {
		srv := pgFixtureServer(body)
		if v, err := newTestPostgresFeed(srv).latest(context.Background(), mustV(t, "16.2")); err == nil {
			t.Errorf("%s: latest = %s, want an error", name, v.latest)
		}
		srv.Close()
	}
}
