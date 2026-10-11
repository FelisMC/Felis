// Package heartbeatsettings is the watchdog's heartbeat URL as the Owner sets it in the
// panel: a dead man's switch at a monitoring service, which alerts when the watchdog's
// pings stop. It wins over /etc/felis/watchdog-heartbeat-url, and the watchdog caches it
// in its state so a run while the database is out still pings it.
package heartbeatsettings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Key is the platform_settings row.
const Key = "watchdog_heartbeat"

// Settings is the stored row.
type Settings struct {
	URL string `json:"url"`
}

// Check accepts an http(s) URL with a host. Its error leaves the URL out: the path is
// the check's key, which anyone who reads it can ping in the host's name.
func Check(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(s, " \t\r\n") {
		return errors.New("the heartbeat URL is not an http:// or https:// URL")
	}
	return nil
}

// Redact is a heartbeat URL as logs and the panel show it: its scheme and host.
func Redact(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "(the heartbeat URL)"
	}
	return u.Scheme + "://" + u.Host + "/..."
}

// Parse decodes a stored row; a missing row or JSON null is nil, nil.
func Parse(raw []byte) (*Settings, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("heartbeat setting: stored row does not parse: %w", err)
	}
	if err := Check(s.URL); err != nil {
		return nil, fmt.Errorf("heartbeat setting: %w", err)
	}
	return &s, nil
}

// Read loads the row through a plain database handle; nil, nil when the panel has none.
func Read(ctx context.Context, db *sql.DB) (*Settings, error) {
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT value::text FROM platform_settings WHERE key = $1`, Key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}
