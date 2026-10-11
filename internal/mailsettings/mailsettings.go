// Package mailsettings is the mail relay the Owner configures in the panel. It lives in
// one platform_settings row so every sender reads the same relay: felis-api (codes and
// notices), the reaper's pre-deletion notices, the host watchdog's alerts and the
// break-glass console's codes. While the row is absent each of them falls back to the
// [smtp] relay `felis setup` wrote into felis.toml, so an install that never opened the
// panel keeps working; once the Owner saves a relay here, it wins everywhere.
//
// The row holds the relay password. It is never sent back to the panel (the API answers
// password_set), never written to the audit log, and leaves the host only inside the
// database's own backups, whose off-site copies are encrypted.
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

// Key is the platform_settings row.
const Key = "smtp"

// AllowedPorts are the submission ports a relay may use. A relay on any other port is
// refused: the API dials it from inside the cluster, and an arbitrary port would turn
// the relay test into a probe of internal services.
var AllowedPorts = []int{25, 465, 587, 2525}

// Settings is one relay.
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

// Validate checks the fields a relay needs. It does not dial: Test does.
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

// SMTP is the relay as the mail package sends through it.
func (s Settings) SMTP() *felismail.SMTP {
	// The relay is whatever the Owner typed; it is dialled with the sender's reach.
	return &felismail.SMTP{Host: s.Host, Port: s.Port, From: s.From, Username: s.Username, Password: s.Password, RequireTLS: s.RequireTLS, Control: netguard.RefuseLinkLocal}
}

// Parse reads a stored row. A JSON null is a cleared row: the panel gave the relay back
// to felis.toml, which Parse reports as nil like a missing row.
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

// Read loads the row through a plain database handle, for the senders outside felis-api.
// It is nil, nil when the panel has no relay.
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

// Clear hands the relay back to felis.toml: `felis setup` calls it after writing [smtp],
// so the relay just typed at the terminal is the one in force rather than an older one
// the panel saved. It sets the row to JSON null, as the panel does.
func Clear(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE platform_settings SET value = 'null'::jsonb, updated_at = now() WHERE key = $1 AND value <> 'null'::jsonb`, Key)
	return err
}
