// Package watchdog is the consumer the platform's health signals otherwise lack:
// `felis watchdog` runs on the host from a systemd timer, checks the control
// plane, the login gate, the fleet, PostgreSQL, the database backups and the
// node, and mails the platform owners when something has stayed wrong long
// enough to matter, again every day while it stays wrong, and once more when it
// clears.
//
// It runs on the host rather than in the cluster so the failures that take the
// cluster down (k3s stopped, the API server wedged, the node out of disk) are
// still reported: the one thing it needs to send mail, the relay, it reaches
// directly, with the credentials it cached on its last good run.
//
// This file is the pure part: turning one run's findings into state changes and
// a message. The probes that produce findings live in probes.go.
package watchdog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Severity orders how urgent a finding is.
type Severity string

const (
	Critical Severity = "critical"
	Warning  Severity = "warning"
)

// Finding is one condition a probe saw on this run.
type Finding struct {
	// Key identifies the condition across runs, e.g. "deployment/felis-api". A
	// key is also how a condition is found again once it clears.
	Key      string   `json:"key"`
	Severity Severity `json:"severity"`
	// Summary and SummaryEN name what is wrong, in Chinese and English.
	Summary   string `json:"summary"`
	SummaryEN string `json:"summary_en"`
	// Hint says where to look next (a command, a troubleshooting section).
	Hint string `json:"hint,omitempty"`
	// For is how long the condition must hold before it is mailed, so a rollout
	// or a restart that heals itself never pages anyone.
	For time.Duration `json:"for"`
	// Event marks a one-off (a failed Job): mailed once, with no reminder and no
	// resolved notice when it goes away.
	Event bool `json:"event,omitempty"`
}

// Report is one run's observations.
type Report struct {
	Findings []Finding
	// Unknown lists key prefixes no probe could evaluate on this run (the API
	// server being down hides every cluster check). Alerts under them keep their
	// state rather than reading as cleared.
	Unknown []string
}

// Alert is a finding the state file remembers across runs.
type Alert struct {
	Finding
	FirstSeen time.Time `json:"first_seen"`
	// Notified is when the alert was last mailed, at NotifiedSeverity; zero
	// while it is pending.
	Notified         time.Time `json:"notified,omitzero"`
	NotifiedSeverity Severity  `json:"notified_severity,omitempty"`
	// ClearedAt is when a mailed alert was first seen gone. It is mailed as
	// resolved only after staying gone for resolveAfter, so a value hovering at
	// its threshold does not mail on every crossing.
	ClearedAt time.Time `json:"cleared_at,omitzero"`
}

// State is what the watchdog keeps between runs.
type State struct {
	Alerts map[string]*Alert `json:"alerts"`
	// Recipients and SMTPPassword are cached from the last run that could read
	// them (the database and the felis-smtp Secret), so an outage of either can
	// still be mailed.
	Recipients   []string `json:"recipients,omitempty"`
	SMTPPassword string   `json:"smtp_password,omitempty"`
}

const (
	// remindEvery is how often a firing alert is mailed again.
	remindEvery = 24 * time.Hour
	// resolveAfter is how long a mailed alert must stay gone before it is
	// mailed as resolved.
	resolveAfter = 10 * time.Minute
)

// Plan is what one run has to tell the owners.
type Plan struct {
	Firing    []Alert // newly past their For, or worse than when last mailed
	Reminders []Alert // still firing a day after the last mail
	Resolved  []Alert // gone for resolveAfter
	// Active is every mailed condition still firing (one-off events aside), for
	// the message's summary.
	Active []Alert
}

// Empty reports whether the plan has nothing to mail.
func (p Plan) Empty() bool {
	return len(p.Firing) == 0 && len(p.Reminders) == 0 && len(p.Resolved) == 0
}

// Observe folds a report into s and returns what is due to be mailed. It only
// tracks when conditions appeared and cleared; Commit records the plan as
// delivered. A run whose mail failed, or that falls in a quiet period, saves
// the state without committing, and the same alerts come due again next run.
func (s *State) Observe(r Report, now time.Time) Plan {
	if s.Alerts == nil {
		s.Alerts = map[string]*Alert{}
	}
	seen := map[string]bool{}
	var plan Plan
	for _, f := range r.Findings {
		seen[f.Key] = true
		a, ok := s.Alerts[f.Key]
		if !ok {
			a = &Alert{FirstSeen: now}
			s.Alerts[f.Key] = a
		}
		a.Finding = f
		a.ClearedAt = time.Time{}
		notified := !a.Notified.IsZero()
		switch {
		case !notified && now.Sub(a.FirstSeen) >= f.For:
			plan.Firing = append(plan.Firing, *a)
		case notified && f.Severity == Critical && a.NotifiedSeverity != Critical:
			// Worse than when it was mailed (a disk from low to nearly full):
			// new news, mailed at once.
			plan.Firing = append(plan.Firing, *a)
		case notified && !f.Event && now.Sub(a.Notified) >= remindEvery:
			plan.Reminders = append(plan.Reminders, *a)
		}
	}
	for key, a := range s.Alerts {
		if seen[key] || underAny(key, r.Unknown) {
			continue
		}
		switch {
		case a.Notified.IsZero() || a.Event:
			// Never mailed, or a one-off: nothing to take back.
			delete(s.Alerts, key)
		case a.ClearedAt.IsZero():
			a.ClearedAt = now
		case now.Sub(a.ClearedAt) >= resolveAfter:
			plan.Resolved = append(plan.Resolved, *a)
		}
	}
	for _, a := range s.Alerts {
		if !a.Notified.IsZero() && a.ClearedAt.IsZero() && !a.Event {
			plan.Active = append(plan.Active, *a)
		}
	}
	for _, l := range [][]Alert{plan.Firing, plan.Reminders, plan.Resolved, plan.Active} {
		sortAlerts(l)
	}
	return plan
}

// Commit records plan as mailed at now.
func (s *State) Commit(plan Plan, now time.Time) {
	for _, l := range [][]Alert{plan.Firing, plan.Reminders} {
		for _, a := range l {
			if cur := s.Alerts[a.Key]; cur != nil {
				cur.Notified, cur.NotifiedSeverity = now, a.Severity
			}
		}
	}
	for _, a := range plan.Resolved {
		delete(s.Alerts, a.Key)
	}
}

func underAny(key string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// sortAlerts puts critical alerts first, then orders by key.
func sortAlerts(l []Alert) {
	sort.Slice(l, func(i, j int) bool {
		if (l[i].Severity == Critical) != (l[j].Severity == Critical) {
			return l[i].Severity == Critical
		}
		return l[i].Key < l[j].Key
	})
}

// Message renders the plan as one mail: a subject naming the counts and host,
// and a body listing what fired, what is still wrong and what cleared.
func (p Plan) Message(host string, now time.Time) (subject, body string) {
	var parts, partsEN []string
	if n := len(p.Firing) + len(p.Reminders); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 项异常", n))
		partsEN = append(partsEN, fmt.Sprintf("%d firing", n))
	}
	if n := len(p.Resolved); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 项恢复", n))
		partsEN = append(partsEN, fmt.Sprintf("%d resolved", n))
	}
	tag := "告警"
	if len(p.Firing)+len(p.Reminders) == 0 {
		tag = "恢复"
	} else if hasCritical(p.Firing) || hasCritical(p.Reminders) {
		tag = "严重告警"
	}
	subject = fmt.Sprintf("Felis %s（%s）：%s · %s", tag, host, strings.Join(parts, "，"), strings.Join(partsEN, ", "))

	var b strings.Builder
	fmt.Fprintf(&b, "Felis 平台巡检 / platform watchdog — %s, %s\r\n", host, now.UTC().Format("2006-01-02 15:04 UTC"))
	section := func(title string, l []Alert, since bool) {
		if len(l) == 0 {
			return
		}
		fmt.Fprintf(&b, "\r\n%s\r\n", title)
		for _, a := range l {
			fmt.Fprintf(&b, "\r\n[%s] %s\r\n    %s\r\n", a.Severity, a.Summary, a.SummaryEN)
			if since {
				fmt.Fprintf(&b, "    自 / since %s\r\n", a.FirstSeen.UTC().Format("2006-01-02 15:04 UTC"))
			}
			if a.Hint != "" {
				fmt.Fprintf(&b, "    → %s\r\n", a.Hint)
			}
		}
	}
	section("== 新出现的异常 / new ==", p.Firing, true)
	section("== 仍未恢复（每日提醒）/ still firing (daily reminder) ==", p.Reminders, true)
	section("== 已恢复 / resolved ==", p.Resolved, false)
	if others := without(p.Active, p.Firing, p.Reminders); len(others) > 0 {
		fmt.Fprintf(&b, "\r\n== 其他仍在进行的告警 / also still firing ==\r\n")
		for _, a := range others {
			fmt.Fprintf(&b, "  [%s] %s / %s\r\n", a.Severity, a.Summary, a.SummaryEN)
		}
	}
	// The journal rather than a -dry-run: the unit carries the flags (proxy port,
	// disk paths) a bare command line would not.
	fmt.Fprintf(&b, "\r\n在主机上运行 `journalctl -u felis-watchdog -n 40` 查看最近一次巡检的全部检查结果。\r\n"+
		"Run `journalctl -u felis-watchdog -n 40` on the host for every check of the latest run.\r\n")
	return subject, b.String()
}

func hasCritical(l []Alert) bool {
	for _, a := range l {
		if a.Severity == Critical {
			return true
		}
	}
	return false
}

// without returns the alerts of all not keyed in any of skip.
func without(all []Alert, skip ...[]Alert) []Alert {
	keys := map[string]bool{}
	for _, l := range skip {
		for _, a := range l {
			keys[a.Key] = true
		}
	}
	var out []Alert
	for _, a := range all {
		if !keys[a.Key] {
			out = append(out, a)
		}
	}
	return out
}

// LoadState reads the state file; a missing file is a fresh state.
func LoadState(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{Alerts: map[string]*Alert{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Alerts == nil {
		s.Alerts = map[string]*Alert{}
	}
	return &s, nil
}

// SaveState writes s atomically, readable by root only: it caches the relay
// password.
func SaveState(path string, s *State) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// QuietUntil reads the maintenance marker the installer writes while it
// restarts things on purpose: a Unix timestamp, before which nothing is mailed.
// A missing or unreadable marker means no quiet period.
func QuietUntil(path string) time.Time {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	var sec int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(raw)), &sec); err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
