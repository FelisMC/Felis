// Package archivepolicy is the Owner's backup and audit retention, set in the panel. Each
// key overrides the matching [archive] or [audit] key of felis.toml for every process
// that applies it: felis-api (the owners' backup limits, the scheduled restore points,
// the backups it records, the audit pruning), the backup Job (how many backups of each
// kind a server keeps and for how long), and the reaper (how long a released world's
// archive is kept). With no row, felis.toml stands.
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

// Key is the platform_settings row.
const Key = "archive_policy"

// Policy is the retention the panel edits, in whole units.
type Policy struct {
	// ManualKeep and ManualRetentionDays bound an owner's own backups of one server;
	// ManualCooldownMinutes is the shortest gap between two of them.
	ManualKeep            int `json:"manualKeep"`
	ManualRetentionDays   int `json:"manualRetentionDays"`
	ManualCooldownMinutes int `json:"manualCooldownMinutes"`
	// ScheduledEveryHours spaces the restore points felis-api takes of played worlds (0
	// turns them off); ScheduledKeep and ScheduledRetentionDays bound them.
	ScheduledEveryHours    int `json:"scheduledEveryHours"`
	ScheduledKeep          int `json:"scheduledKeep"`
	ScheduledRetentionDays int `json:"scheduledRetentionDays"`
	// ReleasedRetentionDays is how long the archive of a released world is kept.
	ReleasedRetentionDays int `json:"releasedRetentionDays"`
	// MaxStoreGiB is the backup store's soft cap: owners' backups pause at it (0 = none).
	MaxStoreGiB int `json:"maxStoreGiB"`
	// AuditRetentionDays is how long audit rows are kept (0 keeps every row).
	AuditRetentionDays int `json:"auditRetentionDays"`
}

const day = 24 * time.Hour

// Validate checks every key against the bounds the panel shows.
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

// FromConfig is the policy felis.toml amounts to, which the panel shows until the Owner
// saves one. Spans are rounded to the unit the panel edits.
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

// Apply overrides rc with the policy's keys.
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

// Parse decodes a stored row. A missing row or JSON null (the panel handed retention
// back to felis.toml) is nil, nil.
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

// Read loads the row through a plain database handle, for the processes outside
// felis-api. It is nil, nil when the panel has set none.
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

// ApplyFrom overrides rc with the panel's policy, if one is set. A row that cannot be read
// leaves felis.toml in force and is returned, for the caller to report.
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
