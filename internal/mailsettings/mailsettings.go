// Package mailsettings stores the shared relay override; absent or cleared settings fall back to felis.toml.
package mailsettings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	felismail "felis.lolicon.best/internal/mail"
	"felis.lolicon.best/internal/netguard"
)

const Key = "smtp"

// AllowedPorts restricts submission ports to prevent probing arbitrary internal services.
var AllowedPorts = []int{25, 465, 587, 2525}

type Settings struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	From     string `json:"from"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// RequireTLS refuses a relay on a port other than 465 that does not offer STARTTLS.
	RequireTLS bool `json:"require_tls"`
	// MaxPerHour caps the mail felis-api sends install-wide; 0 is the default cap.
	MaxPerHour int `json:"max_per_hour,omitempty"`
}

func (s Settings) Validate() error {
	host := strings.TrimSpace(s.Host)
	if host == "" || host != s.Host || strings.ContainsAny(host, " /\\@") || len(host) > 253 {
		return errors.New("the relay host must be a bare host name or address")
	}
	allowed := false
	for _, p := range AllowedPorts {
		allowed = allowed || p == s.Port
	}
	if !allowed {
		return fmt.Errorf("the relay port must be one of 25, 465, 587 or 2525")
	}
	addr, err := mail.ParseAddress(s.From)
	if err != nil || addr.Address != s.From || addr.Name != "" {
		return errors.New("the sender must be a plain email address, like noreply@example.com")
	}
	if len(s.Username) > 256 || len(s.Password) > 1024 || strings.ContainsAny(s.Username, "\r\n") {
		return errors.New("the user name or password is too long or spans lines")
	}
	if s.MaxPerHour < 0 || s.MaxPerHour > 100000 {
		return errors.New("the hourly mail cap must be between 0 (the default) and 100000")
	}
	return nil
}

func (s Settings) SMTP() *felismail.SMTP {
	return &felismail.SMTP{Host: s.Host, Port: s.Port, From: s.From, Username: s.Username, Password: s.Password, RequireTLS: s.RequireTLS, Control: netguard.RefuseLinkLocal}
}

// Parse returns nil for an absent or cleared relay.
func Parse(raw []byte) (*Settings, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("mail settings: stored row does not parse: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("mail settings: stored relay is invalid: %w", err)
	}
	return &s, nil
}

// Read returns nil when no panel relay is configured.
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

// Clear removes the panel override so the relay in felis.toml takes effect.
func Clear(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE platform_settings SET value = 'null'::jsonb, updated_at = now() WHERE key = $1 AND value <> 'null'::jsonb`, Key)
	return err
}
