// Package watchdog runs host health checks and maintains owner alerts across cluster outages.
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

type Severity string

const (
	Critical Severity = "critical"
	Warning  Severity = "warning"
)

type Finding struct {
	// Key identifies a condition across runs.
	Key       string   `json:"key"`
	Severity  Severity `json:"severity"`
	Summary   string   `json:"summary"`
	SummaryEN string   `json:"summary_en"`
	Hint      string   `json:"hint,omitempty"`
	// For delays notification until a condition persists this long.
	For time.Duration `json:"for"`
	// Event is mailed once, without reminders or recovery notices.
	Event bool `json:"event,omitempty"`
}

type Report struct {
	Findings []Finding
	// Unknown prefixes preserve alert state when their probes cannot run.
	Unknown []string
}

type Alert struct {
	Finding
	FirstSeen time.Time `json:"first_seen"`
	// Notified is zero until the alert is mailed.
	Notified         time.Time `json:"notified,omitzero"`
	NotifiedSeverity Severity  `json:"notified_severity,omitempty"`
	// ClearedAt starts the resolveAfter delay before a recovery notice.
	ClearedAt time.Time `json:"cleared_at,omitzero"`
}

type State struct {
	Alerts map[string]*Alert `json:"alerts"`
	// Cache recipients and relay credentials so database or Secret outages can still be reported.
	Recipients   []string `json:"recipients,omitempty"`
	SMTPPassword string   `json:"smtp_password,omitempty"`
	// Relay caches the last readable SMTP configuration.
	Relay *Relay `json:"relay,omitempty"`
	// RelayFromPanel preserves the panel override when the database is unavailable.
	RelayFromPanel bool `json:"relay_from_panel,omitempty"`
	// Heartbeat caches the panel URL so database outages do not stop pings.
	Heartbeat string `json:"heartbeat,omitempty"`
}

type Relay struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	From       string `json:"from"`
	Username   string `json:"username,omitempty"`
	RequireTLS bool   `json:"require_tls"`
}

// Open excludes one-off events and includes notified alerts still firing.
func (s *State) Open() bool {
	for _, a := range s.Alerts {
		if !a.Notified.IsZero() && a.ClearedAt.IsZero() && !a.Event {
			return true
		}
	}
	return false
}

const (
	remindEvery  = 24 * time.Hour
	resolveAfter = 10 * time.Minute
)

type Plan struct {
	Firing    []Alert // newly past their For, or worse than when last mailed
	Reminders []Alert // still firing a day after the last mail
	Resolved  []Alert // gone for resolveAfter
	Active    []Alert
}

func (p Plan) Empty() bool {
	return len(p.Firing) == 0 && len(p.Reminders) == 0 && len(p.Resolved) == 0
}

// Observe updates conditions without marking delivery; Commit only after successful mail so failures retry.
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

func sortAlerts(l []Alert) {
	sort.Slice(l, func(i, j int) bool {
		if (l[i].Severity == Critical) != (l[j].Severity == Critical) {
			return l[i].Severity == Critical
		}
		return l[i].Key < l[j].Key
	})
}

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
	// Read the unit journal to include the actual configured flags.
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

var ErrBadState = errors.New("watchdog: the state file is not one the watchdog wrote")

// LoadState returns a fresh state for a missing file.
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
		return nil, fmt.Errorf("%w: %s: %v", ErrBadState, path, err)
	}
	if s.Alerts == nil {
		s.Alerts = map[string]*Alert{}
	}
	return &s, nil
}

// RecoverState moves malformed state aside and returns its path, or an empty path if unchanged.
func RecoverState(path string, now time.Time) (s *State, aside string, err error) {
	s, err = LoadState(path)
	if !errors.Is(err, ErrBadState) {
		return s, "", err
	}
	aside = fmt.Sprintf("%s.unreadable-%d", path, now.Unix())
	if rerr := os.Rename(path, aside); rerr != nil {
		return nil, "", fmt.Errorf("%w (and could not move it aside: %v)", err, rerr)
	}
	return &State{Alerts: map[string]*Alert{}}, aside, nil
}

// SaveState writes atomically with root-only permissions because state contains the relay password.
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

// FallbackStatePath uses tmpfs when persistent state cannot be written.
const FallbackStatePath = "/run/felis/watchdog-state.json"

// NewestState prefers the fallback only when it is newer than path.
func NewestState(path, fallback string) string {
	fb, err := os.Stat(fallback)
	if err != nil {
		return path
	}
	if st, err := os.Stat(path); err == nil && !fb.ModTime().After(st.ModTime()) {
		return path
	}
	return fallback
}

// SaveStateOr falls back on write failure and still returns the primary path error.
func SaveStateOr(path, fallback string, s *State) error {
	err := SaveState(path, s)
	if err == nil {
		if fallback != "" {
			os.Remove(fallback)
		}
		return nil
	}
	if fallback == "" {
		return err
	}
	if ferr := SaveState(fallback, s); ferr != nil {
		return fmt.Errorf("%w; nor in %s: %v", err, fallback, ferr)
	}
	return fmt.Errorf("%w; kept in %s until the host restarts", err, fallback)
}

// QuietUntil reads a Unix timestamp; missing or unreadable markers disable the quiet period.
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
