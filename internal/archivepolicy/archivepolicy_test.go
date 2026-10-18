package archivepolicy

import (
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/reaper"
)

func valid() Policy {
	return Policy{ManualKeep: 5, ManualRetentionDays: 30, ManualCooldownMinutes: 10, ScheduledEveryHours: 24, ScheduledKeep: 7, ScheduledRetentionDays: 90, ReleasedRetentionDays: 90, MaxStoreGiB: 0, AuditRetentionDays: 365}
}

func TestValidateBounds(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("a valid policy = %v", err)
	}
	ok := []func(*Policy){
		func(p *Policy) { p.ManualCooldownMinutes = 0 },
		func(p *Policy) { p.ScheduledEveryHours = 0 },
		func(p *Policy) { p.MaxStoreGiB = 0 },
		func(p *Policy) { p.AuditRetentionDays = 0 },
		func(p *Policy) { p.AuditRetentionDays = 30 },
		func(p *Policy) { p.ManualKeep, p.ScheduledKeep = 100, 1 },
	}
	for i, change := range ok {
		p := valid()
		change(&p)
		if err := p.Validate(); err != nil {
			t.Errorf("ok %d: %v", i, err)
		}
	}
	bad := map[string]func(*Policy){
		"manualKeep":             func(p *Policy) { p.ManualKeep = 0 },
		"manualKeep high":        func(p *Policy) { p.ManualKeep = 101 },
		"manualRetentionDays":    func(p *Policy) { p.ManualRetentionDays = 0 },
		"manualCooldownMinutes":  func(p *Policy) { p.ManualCooldownMinutes = 1441 },
		"scheduledEveryHours":    func(p *Policy) { p.ScheduledEveryHours = 721 },
		"scheduledKeep":          func(p *Policy) { p.ScheduledKeep = 0 },
		"scheduledRetentionDays": func(p *Policy) { p.ScheduledRetentionDays = 3651 },
		"releasedRetentionDays":  func(p *Policy) { p.ReleasedRetentionDays = 0 },
		"maxStoreGiB":            func(p *Policy) { p.MaxStoreGiB = -1 },
		"auditRetentionDays":     func(p *Policy) { p.AuditRetentionDays = 29 },
	}
	for name, change := range bad {
		p := valid()
		change(&p)
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), strings.Fields(name)[0]) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestFromConfigAndApplyRoundTrip(t *testing.T) {
	rc := reaper.DefaultConfig()
	rc.MaxLocalBytes = 3<<30 + 1
	p := FromConfig(rc, 400*day)
	want := Policy{ManualKeep: 5, ManualRetentionDays: 30, ManualCooldownMinutes: 10, ScheduledEveryHours: 24, ScheduledKeep: 7, ScheduledRetentionDays: 90, ReleasedRetentionDays: 90, MaxStoreGiB: 4, AuditRetentionDays: 400}
	if p != want {
		t.Fatalf("from config = %+v\nwant %+v", p, want)
	}
	p = Policy{ManualKeep: 2, ManualRetentionDays: 3, ManualCooldownMinutes: 4, ScheduledEveryHours: 6, ScheduledKeep: 7, ScheduledRetentionDays: 8, ReleasedRetentionDays: 9, MaxStoreGiB: 10, AuditRetentionDays: 0}
	got := reaper.DefaultConfig()
	p.Apply(&got)
	if got.ManualKeep != 2 || got.ManualRetention != 3*day || got.ManualCooldown != 4*time.Minute || got.ScheduledEvery != 6*time.Hour ||
		got.ScheduledKeep != 7 || got.ScheduledRetention != 8*day || got.Retention != 9*day || got.MaxLocalBytes != 10<<30 {
		t.Fatalf("applied = %+v", got)
	}
	if got.VerifyEvery != reaper.DefaultConfig().VerifyEvery || len(got.WarnBefore) != 2 {
		t.Errorf("apply touched a key the panel does not set: %+v", got)
	}
	if p.Audit() != 0 || valid().Audit() != 365*day {
		t.Errorf("audit = %v / %v", p.Audit(), valid().Audit())
	}
}

func TestParse(t *testing.T) {
	for _, raw := range []string{"", "null", " null "} {
		if p, err := Parse([]byte(raw)); p != nil || err != nil {
			t.Errorf("Parse(%q) = %v, %v", raw, p, err)
		}
	}
	if _, err := Parse([]byte(`{"manualKeep":0}`)); err == nil {
		t.Error("an invalid row parsed")
	}
	if _, err := Parse([]byte(`{`)); err == nil {
		t.Error("a broken row parsed")
	}
	p, err := Parse([]byte(`{"manualKeep":5,"manualRetentionDays":30,"manualCooldownMinutes":10,"scheduledEveryHours":24,"scheduledKeep":7,"scheduledRetentionDays":90,"releasedRetentionDays":90,"maxStoreGiB":0,"auditRetentionDays":365}`))
	if err != nil || p == nil || *p != valid() {
		t.Fatalf("Parse = %+v, %v", p, err)
	}
}
