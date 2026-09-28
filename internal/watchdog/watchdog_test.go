package watchdog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func finding(key string, sev Severity, forDur time.Duration) Finding {
	return Finding{Key: key, Severity: sev, For: forDur, Summary: key + " 中文", SummaryEN: key + " en"}
}

func keys(l []Alert) []string {
	var out []string
	for _, a := range l {
		out = append(out, a.Key)
	}
	return out
}

// run observes r at now and commits whatever came due, like a run whose mail
// went out.
func run(s *State, r Report, now time.Time) Plan {
	p := s.Observe(r, now)
	s.Commit(p, now)
	return p
}

// TestObserveLifecycle walks one condition through pending, firing, the daily
// reminder, clearing, and the resolved notice after it stayed clear.
func TestObserveLifecycle(t *testing.T) {
	s := &State{}
	f := finding("deployment/felis-api", Critical, 5*time.Minute)
	down := Report{Findings: []Finding{f}}

	if p := run(s, down, t0); !p.Empty() {
		t.Fatalf("first sight mailed %+v, want it pending for 5m", p)
	}
	if p := run(s, down, t0.Add(4*time.Minute)); !p.Empty() {
		t.Fatalf("4m in mailed %v", keys(p.Firing))
	}
	p := run(s, down, t0.Add(6*time.Minute))
	if len(p.Firing) != 1 || p.Firing[0].Key != f.Key || !p.Firing[0].FirstSeen.Equal(t0) {
		t.Fatalf("6m in: firing = %+v, want %s since t0", p.Firing, f.Key)
	}
	if p := run(s, down, t0.Add(8*time.Minute)); !p.Empty() {
		t.Fatalf("already mailed, mailed again: %+v", p)
	}
	p = run(s, down, t0.Add(6*time.Minute+remindEvery))
	if len(p.Reminders) != 1 || len(p.Firing) != 0 {
		t.Fatalf("a day later: %+v, want one reminder", p)
	}

	up := Report{}
	later := t0.Add(7*time.Minute + remindEvery)
	if p := run(s, up, later); !p.Empty() {
		t.Fatalf("just cleared, mailed %+v; want resolveAfter to pass first", p)
	}
	p = run(s, up, later.Add(resolveAfter))
	if len(p.Resolved) != 1 || p.Resolved[0].Key != f.Key {
		t.Fatalf("resolved = %v, want %s", keys(p.Resolved), f.Key)
	}
	if len(s.Alerts) != 0 {
		t.Errorf("state after resolve = %v, want empty", s.Alerts)
	}
}

// TestObserveFlapWithinResolveWindow: a condition that returns before
// resolveAfter is neither resolved nor mailed as new.
func TestObserveFlapWithinResolveWindow(t *testing.T) {
	s := &State{}
	f := finding("disk//", Warning, 0)
	run(s, Report{Findings: []Finding{f}}, t0)
	run(s, Report{}, t0.Add(time.Minute))
	if p := run(s, Report{Findings: []Finding{f}}, t0.Add(3*time.Minute)); !p.Empty() {
		t.Fatalf("flap mailed %+v", p)
	}
	if a := s.Alerts[f.Key]; a == nil || !a.ClearedAt.IsZero() {
		t.Fatalf("alert after flap = %+v, want firing again", a)
	}
}

// TestObservePendingNeverMailed: a condition that heals inside its For is
// dropped without a word.
func TestObservePendingNeverMailed(t *testing.T) {
	s := &State{}
	run(s, Report{Findings: []Finding{finding("deployment/registry", Critical, 5*time.Minute)}}, t0)
	if p := run(s, Report{}, t0.Add(2*time.Minute)); !p.Empty() || len(s.Alerts) != 0 {
		t.Fatalf("healed pending alert: plan %+v state %v", p, s.Alerts)
	}
}

// TestObserveEscalation: a warning that turns critical is mailed again at once.
func TestObserveEscalation(t *testing.T) {
	s := &State{}
	run(s, Report{Findings: []Finding{finding("disk//", Warning, 0)}}, t0)
	p := run(s, Report{Findings: []Finding{finding("disk//", Critical, 5*time.Minute)}}, t0.Add(time.Minute))
	if len(p.Firing) != 1 || p.Firing[0].Severity != Critical {
		t.Fatalf("escalation = %+v, want the critical finding mailed", p.Firing)
	}
	if p := run(s, Report{Findings: []Finding{finding("disk//", Critical, 5*time.Minute)}}, t0.Add(2*time.Minute)); !p.Empty() {
		t.Fatalf("critical mailed twice: %+v", p)
	}
}

// TestObserveEventOnce: a failed Job is mailed once, never reminded, and leaves
// without a resolved notice.
func TestObserveEventOnce(t *testing.T) {
	s := &State{}
	ev := finding("job-failed/backup-survival-x", Warning, 0)
	ev.Event = true
	if p := run(s, Report{Findings: []Finding{ev}}, t0); len(p.Firing) != 1 {
		t.Fatalf("event not mailed: %+v", p)
	}
	if p := run(s, Report{Findings: []Finding{ev}}, t0.Add(remindEvery+time.Hour)); !p.Empty() {
		t.Fatalf("event reminded: %+v", p)
	}
	if p := run(s, Report{}, t0.Add(remindEvery+2*time.Hour)); !p.Empty() || len(s.Alerts) != 0 {
		t.Fatalf("event gone: plan %+v state %v, want silent removal", p, s.Alerts)
	}
}

// TestObserveUnknownCarriesOver: while the API server is down, cluster alerts
// neither resolve nor restart their clocks.
func TestObserveUnknownCarriesOver(t *testing.T) {
	s := &State{}
	f := finding("system-server/login", Critical, 0)
	run(s, Report{Findings: []Finding{f}}, t0)
	api := finding("kube-api", Critical, 5*time.Minute)
	for i := 1; i <= 3; i++ {
		p := run(s, Report{Findings: []Finding{api}, Unknown: ClusterPrefixes}, t0.Add(time.Duration(i)*resolveAfter))
		if len(p.Resolved) != 0 {
			t.Fatalf("run %d resolved %v while the API was down", i, keys(p.Resolved))
		}
	}
	if a := s.Alerts[f.Key]; a == nil || !a.ClearedAt.IsZero() {
		t.Fatalf("login alert = %+v, want still firing", a)
	}
}

// TestObserveUncommittedRetries: a plan whose mail failed comes due again.
func TestObserveUncommittedRetries(t *testing.T) {
	s := &State{}
	f := finding("postgres", Critical, 0)
	if p := s.Observe(Report{Findings: []Finding{f}}, t0); len(p.Firing) != 1 {
		t.Fatalf("not due: %+v", p)
	}
	if p := s.Observe(Report{Findings: []Finding{f}}, t0.Add(2*time.Minute)); len(p.Firing) != 1 || !p.Firing[0].FirstSeen.Equal(t0) {
		t.Fatalf("after a failed send: %+v, want the same alert due again since t0", p.Firing)
	}
}

func TestMessage(t *testing.T) {
	s := &State{}
	run(s, Report{Findings: []Finding{finding("memory", Warning, 0)}}, t0)
	p := run(s, Report{Findings: []Finding{finding("memory", Warning, 0), finding("postgres", Critical, 0)}}, t0.Add(time.Minute))
	subject, body := p.Message("node-1", t0.Add(time.Minute))
	for _, want := range []string{"严重告警", "node-1", "1 项异常", "1 firing"} {
		if !strings.Contains(subject, want) {
			t.Errorf("subject %q lacks %q", subject, want)
		}
	}
	for _, want := range []string{"postgres 中文", "postgres en", "== 其他仍在进行的告警 / also still firing ==", "memory 中文 / memory en", "journalctl -u felis-watchdog"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
}

func TestStateRoundTripPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog", "state.json")
	s := &State{SMTPPassword: "secret", Recipients: []string{"owner@example.com"}}
	run(s, Report{Findings: []Finding{finding("memory", Warning, time.Hour)}}, t0)
	if err := SaveState(path, s); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v (%v), want 0600", info.Mode(), err)
	}
	got, err := LoadState(path)
	if err != nil || got.SMTPPassword != "secret" || !got.Alerts["memory"].FirstSeen.Equal(t0) {
		t.Fatalf("LoadState = %+v, %v", got, err)
	}
	fresh, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || fresh.Alerts == nil {
		t.Fatalf("missing state = %+v, %v", fresh, err)
	}
}

func TestQuietUntil(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "quiet")
	if !QuietUntil(marker).IsZero() {
		t.Error("missing marker should mean no quiet period")
	}
	if err := os.WriteFile(marker, []byte("1790000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := QuietUntil(marker); got.Unix() != 1790000000 {
		t.Errorf("QuietUntil = %v", got)
	}
}

// TestRecoverState: a state file that does not parse is moved aside, whole,
// and the run starts over; a good or missing one is loaded as LoadState does.
func TestRecoverState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"alerts": {"memo`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, aside, err := RecoverState(path, t0)
	if err != nil || s == nil || s.Alerts == nil || len(s.Alerts) != 0 {
		t.Fatalf("RecoverState(bad) = %+v, %q, %v; want a fresh state", s, aside, err)
	}
	if want := path + ".unreadable-" + fmt.Sprint(t0.Unix()); aside != want {
		t.Fatalf("aside = %q, want %q", aside, want)
	}
	if raw, err := os.ReadFile(aside); err != nil || string(raw) != `{"alerts": {"memo` {
		t.Fatalf("the moved file = %q, %v; want the bad state kept as it was", raw, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the bad state is still at %s (%v)", path, err)
	}

	good := &State{Recipients: []string{"owner@example.com"}}
	run(good, Report{Findings: []Finding{finding("memory", Warning, 0)}}, t0)
	if err := SaveState(path, good); err != nil {
		t.Fatal(err)
	}
	s, aside, err = RecoverState(path, t0)
	if err != nil || aside != "" || s.Alerts["memory"] == nil || len(s.Recipients) != 1 {
		t.Fatalf("RecoverState(good) = %+v, %q, %v", s, aside, err)
	}
	s, aside, err = RecoverState(filepath.Join(dir, "missing.json"), t0)
	if err != nil || aside != "" || s.Alerts == nil {
		t.Fatalf("RecoverState(missing) = %+v, %q, %v", s, aside, err)
	}
}

// TestSaveStateOr: a state file that cannot be written leaves the state in the
// fallback, which the next run reads; once the file takes it again the fallback
// goes, and a fallback older than the file is never read.
func TestSaveStateOr(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	fbDir := filepath.Join(t.TempDir(), "run")
	fallback := filepath.Join(fbDir, "watchdog-state.json")
	before := &State{Recipients: []string{"owner@example.com"}}
	if err := SaveState(path, before); err != nil {
		t.Fatal(err)
	}
	if got := NewestState(path, fallback); got != path {
		t.Fatalf("no fallback yet: NewestState = %q, want the file", got)
	}
	// An earlier run wrote the file, minutes before this one. Linux stamps files
	// from a coarse clock, so two writes a test makes back to back can share an
	// mtime, and a tie reads the file.
	earlier := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, earlier, earlier); err != nil {
		t.Fatal(err)
	}

	mailed := &State{Recipients: []string{"owner@example.com"}}
	run(mailed, Report{Findings: []Finding{finding("memory", Warning, 0)}}, t0)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	err := SaveStateOr(path, fallback, mailed)
	if err == nil || !strings.HasSuffix(err.Error(), "; kept in "+fallback+" until the host restarts") {
		t.Fatalf("SaveStateOr on a read-only directory = %v, want the error saying where the state went", err)
	}
	if got := NewestState(path, fallback); got != fallback {
		t.Fatalf("after a failed save: NewestState = %q, want the fallback", got)
	}
	s, err := LoadState(fallback)
	if err != nil || s.Alerts["memory"] == nil || !s.Alerts["memory"].Notified.Equal(t0) {
		t.Fatalf("fallback = %+v, %v; want the alert mailed at t0", s, err)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveStateOr(path, fallback, s); err != nil {
		t.Fatalf("SaveStateOr once the file is writable: %v", err)
	}
	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Fatalf("the fallback outlived a good save (%v)", err)
	}
	if got := NewestState(path, fallback); got != path {
		t.Fatalf("after a good save: NewestState = %q, want the file", got)
	}

	// A fallback older than the file (one whose removal failed) stays unread.
	if err := SaveState(fallback, before); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fallback, past, past); err != nil {
		t.Fatal(err)
	}
	if got := NewestState(path, fallback); got != path {
		t.Fatalf("stale fallback: NewestState = %q, want the file", got)
	}

	// No fallback: the error is the file's alone, and nothing else is read.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err = SaveStateOr(path, "", mailed)
	if err == nil || strings.Contains(err.Error(), "; ") || NewestState(path, "") != path {
		t.Fatalf("SaveStateOr with no fallback = %v, NewestState = %q", err, NewestState(path, ""))
	}

	// With nowhere to keep it, the error says that too.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fbDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(fbDir, 0o700) })
	err = SaveStateOr(path, fallback, mailed)
	if err == nil || !strings.Contains(err.Error(), "; nor in "+fallback+": ") {
		t.Fatalf("SaveStateOr with both read-only = %v", err)
	}
}

// TestStateOpen: open is a condition the owners were told of that still holds.
func TestStateOpen(t *testing.T) {
	s := &State{}
	f := finding("memory", Warning, time.Hour)
	run(s, Report{Findings: []Finding{f}}, t0)
	if s.Open() {
		t.Fatal("a pending alert, never mailed, counts as open")
	}
	run(s, Report{Findings: []Finding{f}}, t0.Add(time.Hour))
	if !s.Open() {
		t.Fatal("a mailed alert still firing is not open")
	}
	run(s, Report{}, t0.Add(time.Hour+time.Minute))
	if s.Open() {
		t.Fatal("an alert seen gone counts as open")
	}
	ev := finding("watchdog/state", Warning, 0)
	ev.Event = true
	e := &State{}
	run(e, Report{Findings: []Finding{ev}}, t0)
	if e.Open() {
		t.Fatal("a one-off event counts as open")
	}
}
