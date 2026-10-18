// Package heartbeatsettings stores the owner's heartbeat URL, overriding the watchdog file setting.
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

const Key = "watchdog_heartbeat"

type Settings struct {
	URL string `json:"url"`
}

// Check omits the URL from errors because its path may contain a secret.
func Check(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(s, " \t\r\n") {
		return errors.New("the heartbeat URL is not an http:// or https:// URL")
	}
	return nil
}

// Redact exposes only the heartbeat URL's scheme and host.
func Redact(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "(the heartbeat URL)"
	}
	return u.Scheme + "://" + u.Host + "/..."
}

// Parse returns nil for a missing row or JSON null.
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

// Read returns nil when no heartbeat URL is configured.
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
