// Package archivepolicy applies panel retention overrides, falling back to felis.toml when absent.
package archivepolicy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"felis.lolicon.best/internal/reaper"
)

const Key = "archive_policy"

type Policy struct {
	// ManualCooldownMinutes bounds the shortest interval between owner backups.
	ManualKeep            int `json:"manualKeep"`
	ManualRetentionDays   int `json:"manualRetentionDays"`
	ManualCooldownMinutes int `json:"manualCooldownMinutes"`
	// ScheduledEveryHours=0 disables scheduled restore points.
	ScheduledEveryHours    int `json:"scheduledEveryHours"`
	ScheduledKeep          int `json:"scheduledKeep"`
	ScheduledRetentionDays int `json:"scheduledRetentionDays"`
	ReleasedRetentionDays  int `json:"releasedRetentionDays"`
	// MaxStoreGiB is the backup store's soft cap: owners' backups pause at it (0 = none).
	MaxStoreGiB int `json:"maxStoreGiB"`
	// AuditRetentionDays is how long audit rows are kept (0 keeps every row).
	AuditRetentionDays int `json:"auditRetentionDays"`
}

const day = 24 * time.Hour

func (p Policy) Validate() error {
	var bad []string
	check := func(name string, v, lo, hi int) {
		if v < lo || v > hi {
			bad = append(bad, fmt.Sprintf("%s must be %d–%d", name, lo, hi))
		}
	}
	check("manualKeep", p.ManualKeep, 1, 100)
	check("manualRetentionDays", p.ManualRetentionDays, 1, 3650)
	check("manualCooldownMinutes", p.ManualCooldownMinutes, 0, 1440)
	check("scheduledEveryHours", p.ScheduledEveryHours, 0, 720)
	check("scheduledKeep", p.ScheduledKeep, 1, 100)
	check("scheduledRetentionDays", p.ScheduledRetentionDays, 1, 3650)
	check("releasedRetentionDays", p.ReleasedRetentionDays, 1, 3650)
	check("maxStoreGiB", p.MaxStoreGiB, 0, 1<<20)
	if p.AuditRetentionDays != 0 {
		check("auditRetentionDays", p.AuditRetentionDays, 30, 3650)
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	return nil
}

// FromConfig rounds retention spans to panel-editable units.
func FromConfig(rc reaper.Config, audit time.Duration) Policy {
	return Policy{
		ManualKeep:             rc.ManualKeep,
		ManualRetentionDays:    roundTo(rc.ManualRetention, day),
		ManualCooldownMinutes:  roundTo(rc.ManualCooldown, time.Minute),
		ScheduledEveryHours:    roundTo(rc.ScheduledEvery, time.Hour),
		ScheduledKeep:          rc.ScheduledKeep,
		ScheduledRetentionDays: roundTo(rc.ScheduledRetention, day),
		ReleasedRetentionDays:  roundTo(rc.Retention, day),
		MaxStoreGiB:            int((rc.MaxLocalBytes + 1<<30 - 1) >> 30),
		AuditRetentionDays:     roundTo(audit, day),
	}
}

func roundTo(d, unit time.Duration) int {
	return int((d + unit/2) / unit)
}

func (p Policy) Apply(rc *reaper.Config) {
	rc.ManualKeep = p.ManualKeep
	rc.ManualRetention = time.Duration(p.ManualRetentionDays) * day
	rc.ManualCooldown = time.Duration(p.ManualCooldownMinutes) * time.Minute
	rc.ScheduledEvery = time.Duration(p.ScheduledEveryHours) * time.Hour
	rc.ScheduledKeep = p.ScheduledKeep
	rc.ScheduledRetention = time.Duration(p.ScheduledRetentionDays) * day
	rc.Retention = time.Duration(p.ReleasedRetentionDays) * day
	rc.MaxLocalBytes = int64(p.MaxStoreGiB) << 30
}

// Audit is how long audit rows are kept; 0 keeps every row.
func (p Policy) Audit() time.Duration {
	return time.Duration(p.AuditRetentionDays) * day
}

// Parse returns nil for missing or cleared settings.
func Parse(raw []byte) (*Policy, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("retention policy: stored row does not parse: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("retention policy: stored row is invalid: %w", err)
	}
	return &p, nil
}

// Read returns nil when no panel policy is configured.
func Read(ctx context.Context, db *sql.DB) (*Policy, error) {
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

// ApplyFrom preserves configuration and returns an error if the panel override cannot be read.
func ApplyFrom(ctx context.Context, db *sql.DB, rc *reaper.Config) error {
	p, err := Read(ctx, db)
	if err != nil {
		return err
	}
	if p != nil {
		p.Apply(rc)
	}
	return nil
}
